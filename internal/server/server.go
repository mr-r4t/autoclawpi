// Package server menyediakan endpoint OpenAI-compatible yang
// meneruskan request ke proxy inference AutoClaw dengan round-robin multi-akun.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hirotomasato/autoclawpi/internal/client"
	"github.com/hirotomasato/autoclawpi/internal/db"
)

// Server adalah proxy server OpenAI-compatible.
type Server struct {
	cl      *client.Client
	rrIndex int
	mu      sync.Mutex
	apiKey  string
	limiter *RateLimiter
}

// New membuat server baru.
func New(cl *client.Client) *Server {
	return &Server{
		cl:      cl,
		rrIndex: 0,
		limiter: NewRateLimiter(1.0/1.5, 3), // default: 1 req/1.5s, burst 3
	}
}

// WithRateLimit mengatur rate limiter (token/detik + burst).
func (s *Server) WithRateLimit(ratePerSec float64, burst int) *Server {
	if ratePerSec > 0 && burst > 0 {
		s.limiter = NewRateLimiter(ratePerSec, burst)
	}
	return s
}

// WithAPIKey mengatur API key untuk proteksi endpoint.
func (s *Server) WithAPIKey(key string) *Server {
	if key != "" {
		s.apiKey = key
	}
	return s
}

// Handler mengembalikan http.Handler yang menangani route OpenAI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.wrapAuth(s.handleChat))
	mux.HandleFunc("/v1/models", s.wrapAuth(s.handleModels))
	mux.HandleFunc("/healthz", s.handleHealth)
	return mux
}

// wrapAuth melindungi endpoint dengan API key jika dikonfigurasi.
// Menerima key legacy cfg.APIKey (flag -api-key) ATAU salah satu key
// dari daftar api_keys yang dikelola via halaman Settings.
func (s *Server) wrapAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey != "" || hasMultiAPIKeys() {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !validAPIKey(got) {
				writeOpenAIError(w, 401, "invalid_api_key", "API key lokal salah")
				return
			}
		}
		next(w, r)
	}
}

// validAPIKey cek key terhadap key legacy dan daftar api_keys dari DB.
func validAPIKey(got string) bool {
	if got == "" {
		return false
	}
	data, _ := db.GetConfig("api_keys")
	if data != "" {
		var keys []struct {
			Key string `json:"key"`
		}
		if json.Unmarshal([]byte(data), &keys) == nil {
			for _, k := range keys {
				if k.Key == got {
					return true
				}
			}
		}
	}
	return false
}

// hasMultiAPIKeys true jika ada key yang dikelola via Settings.
func hasMultiAPIKeys() bool {
	data, _ := db.GetConfig("api_keys")
	return data != "" && data != "[]"
}

// handleChat menangani /v1/chat/completions.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
	if err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "gagal baca body: "+err.Error())
		return
	}

	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "gagal parse body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeOpenAIError(w, 400, "invalid_request_error", "model required")
		return
	}

	route := client.RouteID(req.Model)
	upstreamBody, err := replaceModel(raw, client.BodyModel(route))
	if err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "gagal olah body: "+err.Error())
		return
	}

	// Round-robin: coba setiap akun hingga salah satu berhasil
	allAccounts, _ := db.ListAccounts()
	if len(allAccounts) == 0 {
		writeOpenAIError(w, 503, "no_accounts", "tidak ada akun tersedia")
		return
	}

	// Pool round-robin: hanya akun aktif dengan token — distribusi tetap merata
	// meski ada akun inactive di database.
	var accounts []db.Account
	for _, a := range allAccounts {
		if a.Active && a.AccessToken != "" {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		writeOpenAIError(w, 503, "no_active_accounts", "tidak ada akun aktif tersedia")
		return
	}

	// Terapkan strategi account selection (bisa diganti live dari Settings)
	accounts = applyStrategy(accounts)

	// Rate limit: tunggu token sebelum menyentuh upstream (hindari WAF block)
	if s.limiter != nil {
		if err := s.limiter.Wait(r.Context()); err != nil {
			writeOpenAIError(w, 503, "rate_limited", "request dibatalkan: "+err.Error())
			return
		}
	}
	s.mu.Lock()
	startIdx := s.rrIndex % len(accounts)
	s.mu.Unlock()
	// rrIndex TIDAK dimajukan di sini — maju setelah diketahui akun mana
	// yang benar-benar melayani (lihat advanceRR di bawah), sehingga akun
	// penampung failover tidak dapat giliran dobel di request berikutnya.
	advanceRR := func(servedIdx int) {
		s.mu.Lock()
		s.rrIndex = (servedIdx + 1) % len(accounts)
		s.mu.Unlock()
	}

	for i := 0; i < len(accounts); i++ {
		idx := (startIdx + i) % len(accounts)
		acct := accounts[idx]

		status, _, body, ferr := s.forward(r.Context(), acct, route, upstreamBody, req.Stream, w)
		if ferr != nil {
			log.Printf("[autoclawpi] akun #%d error: %v", acct.ID, ferr)
			logFailure(acct.ID, route, "network error: "+ferr.Error())
			continue
		}
		// 401 — token expired, coba refresh
		if status == 401 {
			refreshed, rerr := s.refreshToken(&acct)
			if rerr != nil {
				log.Printf("[autoclawpi] akun #%d refresh gagal: %v", acct.ID, rerr)
				logFailure(acct.ID, route, "401 refresh gagal: "+rerr.Error())
				continue
			}
			// Retry dengan token baru
			status, _, _, ferr2 := s.forward(r.Context(), acct, route, upstreamBody, req.Stream, w)
			if ferr2 != nil {
				log.Printf("[autoclawpi] akun #%d retry error: %v", acct.ID, ferr2)
				logFailure(acct.ID, route, "network error (setelah refresh): "+ferr2.Error())
				continue
			}
			_ = refreshed
			if status == 200 {
				db.UpdateAccountUsed(acct.ID)
				advanceRR(idx)
				return
			}
			logFailure(acct.ID, route, fmt.Sprintf("HTTP %d setelah refresh", status))
			continue
		}
		// 403 — WAF block / kuota habis, coba akun berikutnya
		if status == 403 {
			reason := extractUpstreamError(body)
			if reason == "" {
				reason = "WAF block"
			}
			log.Printf("[autoclawpi] akun #%d gagal (403): %s, coba akun berikutnya", acct.ID, reason)
			logFailure(acct.ID, route, "403 "+reason)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if status == 200 {
			// Baris success ditulis oleh logUsage/logUsageStream di forward()
			// — SATU baris per request dengan token usage.
			db.UpdateAccountUsed(acct.ID)
			advanceRR(idx)
			return
		}
		// Status lain (4xx, 5xx) — coba akun berikutnya
		logFailure(acct.ID, route, fmt.Sprintf("HTTP %d %s", status, extractUpstreamError(body)))
	}

	// Semua akun gagal — tiap attempt sudah tercatat sebagai baris error
	writeOpenAIError(w, 503, "all_accounts_failed", "semua akun gagal memproses request")
}

// applyStrategy mengurutkan pool akun sesuai strategi yang tersimpan di DB.
// Strategi dibaca per-request sehingga perubahan dari web panel Settings
// berlaku live tanpa restart server.
//   - round-robin: urutan asli (rotasi index oleh pemanggil)
//   - least-used : akun dengan last_used_at paling lama didahulukan
//   - first      : urutan asli, selalu mulai dari akun pertama (ID terkecil)
func applyStrategy(accounts []db.Account) []db.Account {
	strategy := db.GetStrategy()
	switch strategy {
	case "least-used":
		// Copy supaya tidak mengubah slice pemanggil.
		sorted := make([]db.Account, len(accounts))
		copy(sorted, accounts)
		sort.SliceStable(sorted, func(i, j int) bool {
			ti := lastUsedTime(sorted[i])
			tj := lastUsedTime(sorted[j])
			if ti.Equal(tj) {
				return sorted[i].ID < sorted[j].ID
			}
			return ti.Before(tj)
		})
		return sorted
	default:
		// round-robin & first: urutan asli dari ListAccounts (ORDER BY id)
		return accounts
	}
}

// lastUsedTime parse last_used_at dengan fallback ke created_at / zero time.
func lastUsedTime(a db.Account) time.Time {
	if t, err := time.Parse(time.RFC3339, a.LastUsedAt); err == nil && !t.IsZero() {
		return t
	}
	if t, err := time.Parse(time.RFC3339, a.CreatedAt); err == nil && !t.IsZero() {
		return t
	}
	return time.Time{}
}

// forward mengirim request ke upstream dan menulis response ke klien.
// Untuk response non-streaming, membaca tubuh penuh dulu, parse token usage, lalu log.
// Untuk streaming, melewatkan data langsung tanpa log.
func (s *Server) forward(ctx context.Context, acct db.Account, route string, body []byte, stream bool, w http.ResponseWriter) (int, string, []byte, error) {
	baseURL := s.cl.InferenceBase
	if baseURL == "" {
		baseURL = "https://autoglm-api.autoglm.ai"
	}

	url := baseURL + "/autoclaw-proxy/proxy/autoclaw/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, "", nil, fmt.Errorf("buat request: %w", err)
	}

	headers := s.cl.InferenceHeader(acct.AccessToken, route)
	headers["Content-Type"] = "application/json"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	resp, err := s.cl.Do(req)
	if err != nil {
		return 0, "", nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}

	// 401 — token expired: jangan tulis apa pun ke klien,
	// biarkan handleChat refresh token dan retry.
	if resp.StatusCode == 401 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, ct, b, nil
	}

	// Status error lain (selain 403/WAF): baca body lalu failover.
	if resp.StatusCode >= 400 && resp.StatusCode != 403 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, ct, b, nil
	}

	// 403 bisa berarti hard WAF block (body hanya "forbidden") ATAU
	// WAF prefix + konten valid. Keputusan diambil setelah body diperiksa.
	statusCode := resp.StatusCode

	if stream {
		return s.streamResponse(acct, route, ct, statusCode, resp.Body, w)
	}

	// Non-streaming: baca body, bersihkan WAF prefix, kirim
	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	rawBody = stripWAFPrefixes(rawBody)
	if isWAFBlockOnly(rawBody) {
		// Hard WAF block — return 403 agar handleChat retry akun berikutnya
		log.Printf("[autoclawpi] WAF hard block: %s", truncateResp(rawBody, 80))
		return http.StatusForbidden, ct, rawBody, nil
	}
	if statusCode != 200 {
		if statusCode == 403 {
			// 403 dengan error JSON murni (mis. kuota habis: {"code":810000,
			// "message":"GLM-5.3 free quota used up"}) BUKAN konten valid —
			// failover ke akun berikutnya. Hanya body ber-marker konten
			// (choices/delta/usage) yang dipromosikan jadi 200.
			if hasContentMarkers(rawBody) {
				statusCode = 200
			} else {
				log.Printf("[autoclawpi] akun #%d gagal (403): %s", acct.ID, truncateResp(rawBody, 80))
				return http.StatusForbidden, ct, rawBody, nil
			}
		} else {
			// error upstream lain — jangan tulis ke klien, biarkan failover
			return statusCode, ct, rawBody, nil
		}
	}
	cleaned := stripNonStandard(rawBody)
	if cleaned == nil {
		return http.StatusForbidden, ct, rawBody, nil
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(statusCode)
	w.Write(cleaned)

	// Log token usage
	go logUsage(acct.ID, route, rawBody)

	return statusCode, ct, nil, nil
}

// streamResponse mengalirkan response streaming ke klien.
// Header hanya di-commit SETELAH dipastikan bukan WAF block, sehingga
// failover round-robin tetap bekerja untuk request streaming.
// Token usage juga dicatat ke logs (parsed dari chunk SSE terakhir).
func (s *Server) streamResponse(acct db.Account, route, ct string, statusCode int, body io.Reader, w http.ResponseWriter) (int, string, []byte, error) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var head []byte // buffer awal sebelum header di-commit
	var logBuf bytes.Buffer
	committed := false

	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if committed {
				// Setelah commit: strip WAF prefix per-chunk
				data := stripWAFPrefixes(buf[:n])
				if len(data) > 0 {
					if logBuf.Len() < 4<<20 {
						logBuf.Write(data)
					}
					if _, werr := w.Write(data); werr != nil {
						break
					}
					if flusher != nil {
						flusher.Flush()
					}
				}
			} else {
				head = stripWAFPrefixes(append(head, buf[:n]...))
				if isWAFBlockOnly(head) {
					log.Printf("[autoclawpi] WAF hard block (stream): %s", truncateResp(head, 80))
					return http.StatusForbidden, ct, nil, nil
				}
				// Commit begitu ada tanda konten stream valid, atau EOF,
				// atau buffer melebihi batas (keputusan final).
				if hasStreamStart(head) || rerr != nil || len(head) >= 16*1024 {
					if len(head) == 0 {
						// body kosong — anggap gagal agar failover
						return http.StatusForbidden, ct, nil, nil
					}
					if statusCode == 403 {
						// WAF balas 403 tapi konten valid — teruskan sebagai 200
						statusCode = 200
					} else if statusCode != 200 {
						// error upstream lain — failover tanpa menulis ke klien
						return statusCode, ct, head, nil
					}
					// Commit header + tulis buffer awal
					w.Header().Set("Content-Type", ct)
					w.Header().Set("Cache-Control", "no-cache")
					w.Header().Set("X-Accel-Buffering", "no")
					w.WriteHeader(statusCode)
					committed = true
					logBuf.Write(head)
					if _, werr := w.Write(head); werr != nil {
						break
					}
					if flusher != nil {
						flusher.Flush()
					}
					head = nil
				}
			}
		}
		if rerr != nil {
			break
		}
	}

	if !committed {
		// Tidak pernah mendapat konten yang bisa dikirim — failover
		return http.StatusForbidden, ct, nil, nil
	}

	// Log token usage dari stream
	go logUsageStream(acct.ID, route, logBuf.Bytes())
	return statusCode, ct, nil, nil
}

// stripNonStandard removes non-OpenAI fields from chat completion response.
func stripNonStandard(body []byte) []byte {
	// Strip all WAF prefixes
	body = stripWAFPrefixes(body)
	if len(body) == 0 {
		return nil
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil
	}
	// Remove reasoning_content from choices
	if choices, ok := data["choices"].([]any); ok {
		for _, c := range choices {
			if choice, ok := c.(map[string]any); ok {
				if msg, ok := choice["message"].(map[string]any); ok {
					// Move reasoning_content to content if content is empty
					if content, _ := msg["content"].(string); content == "" {
						if reasoning, ok := msg["reasoning_content"].(string); ok && reasoning != "" {
							msg["content"] = reasoning[:min(len(reasoning), 500)]
						}
					}
					delete(msg, "reasoning_content")
				}
			}
		}
	}
	// Remove non-standard usage details
	if usage, ok := data["usage"].(map[string]any); ok {
		delete(usage, "completion_tokens_details")
		delete(usage, "prompt_tokens_details")
	}
	cleaned, _ := json.Marshal(data)
	return cleaned
}

// logUsage parse response body dan catat ke database.
func logUsage(acctID int64, model string, body []byte) {
	body = stripWAFPrefixes(body)
	var data struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return
	}
	status := "success"
	errMsg := ""
	if data.Error != nil {
		status = "error"
		errMsg = data.Error.Message
	}
	pts := 0
	cts := 0
	tts := 0
	if data.Usage != nil {
		pts = data.Usage.PromptTokens
		cts = data.Usage.CompletionTokens
		tts = data.Usage.TotalTokens
	}
	cost := float64(tts) * 0.001 / 1000.0
	_, _ = db.AddLog(&db.LogEntry{
		AccountID:        acctID,
		Model:            model,
		PromptTokens:     pts,
		CompletionTokens: cts,
		TotalTokens:      tts,
		Cost:             cost,
		Status:           status,
		Error:            errMsg,
	})
}

// logFailure mencatat kegagalan akun ke Request Logs (status=error) supaya
// penyebab gagal terlihat di web panel (kuota habis, WAF, refresh gagal, dll).
// Baris success TIDAK ditulis di sini — itu tugas logUsage/logUsageStream
// di forward(), sehingga satu request menghasilkan maksimal satu baris success.
func logFailure(acctID int64, model, reason string) {
	_, _ = db.AddLog(&db.LogEntry{
		AccountID: acctID,
		Model:     model,
		Status:    "error",
		Error:     reason,
	})
}

// extractUpstreamError mengambil pesan error dari body upstream
// (mis. {"message":"GLM-5.3 free quota used up..."}).
func extractUpstreamError(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var e struct {
		Message string `json:"message"`
		Error   string `json:"error"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return ""
	}
	if e.Error != "" {
		return e.Error
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Msg
}

// handleModels menangani /v1/models.
// Daftar model REAL-TIME: tiap kandidat di-probe ke upstream chat dengan
// body minimal. Response mengklasifikasi validitas:
//   200           -> model valid
//   401/403       -> model dikenali tapi masalah akun/kuota -> tetap valid
//   400 非法模型   -> model tidak dikenal upstream -> buang
// Hasil di-cache 10 menit agar tidak membakar kuota tiap buka halaman.
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	models := s.realtimeModels()
	writeJSON(w, 200, map[string]any{"object": "list", "data": models})
}

// candidateModels adalah model OpenAI-style yang dikenal autoclawpi.
var candidateModels = []string{
	"auto", "auto-fast", "glm-5.3", "glm-5.3-flash", "glm-5-turbo", "glm-5.2",
	"deepseek-v4-pro", "deepseek-v4-flash",
}

type modelCache struct {
	mu      sync.Mutex
	cond    *sync.Cond
	items   []map[string]any
	fetched time.Time
	probing bool
}

var mcache modelCache

// realtimeModels mengembalikan daftar model yang masih diterima upstream.
// Single-flight: request konkuren saat probe berjalan menunggu hasil yang sama,
// tidak memicu probe kedua.
func (s *Server) realtimeModels() []map[string]any {
	mcache.mu.Lock()
	if mcache.items != nil && time.Since(mcache.fetched) < 10*time.Minute {
		items := mcache.items
		mcache.mu.Unlock()
		return items
	}
	if mcache.probing {
		// Probe sedang berjalan di goroutine lain — tunggu selesai.
		cond := mcache.cond
		if cond == nil {
			cond = sync.NewCond(&mcache.mu)
			mcache.cond = cond
		}
		cond.Wait()
		items := mcache.items
		mcache.mu.Unlock()
		return items
	}
	mcache.probing = true
	mcache.mu.Unlock()
	defer func() {
		mcache.mu.Lock()
		mcache.probing = false
		cond := mcache.cond
		mcache.mu.Unlock()
		if cond != nil {
			cond.Broadcast()
		}
	}()

	type probeResult struct {
		idx   int
		valid bool
	}
	results := make(chan probeResult, len(candidateModels))
	for i, m := range candidateModels {
		go func(idx int, model string) {
			results <- probeResult{idx, s.probeModel(model)}
		}(i, m)
	}
	valid := make([]map[string]any, len(candidateModels))
	validCount := 0
	probeFailed := 0
	for range candidateModels {
		r := <-results
		if r.valid {
			valid[r.idx] = map[string]any{
				"id": candidateModels[r.idx], "object": "model", "created": 1, "owned_by": "autoclaw",
			}
			validCount++
		} else {
			probeFailed++
		}
	}
	compact := make([]map[string]any, 0, validCount)
	for _, m := range valid {
		if m != nil {
			compact = append(compact, m)
		}
	}
	// Semua probe gagal (kemungkinan jaringan/token global) — pakai cache lama
	// atau seluruh kandidat agar layanan tetap tampil.
	if probeFailed == len(candidateModels) {
		mcache.mu.Lock()
		if mcache.items != nil {
			items := mcache.items
			mcache.mu.Unlock()
			return items
		}
		mcache.mu.Unlock()
	}
	mcache.mu.Lock()
	mcache.items = compact
	mcache.fetched = time.Now()
	items := mcache.items
	mcache.mu.Unlock()
	return items
}

// WarmModels menjalankan realtimeModels sekali di background saat startup
// agar klik pertama ke Settings/API Docs tidak menunggu probe upstream.
func (s *Server) WarmModels() {
	go func() { _ = s.realtimeModels() }()
}

// probeModel mengirim request chat minimal ke upstream sebagai akun aktif
// pertama. 400 dengan 非法模型 berarti model tidak dikenal; 401/403 berarti
// model dikenali (gagal karena token/kuota, bukan model).
func (s *Server) probeModel(model string) bool {
	route := client.RouteID(model)
	acct, ok := s.probeAccount()
	if !ok {
		return true // tanpa akun aktif, anggap semua kandidat valid
	}
	baseURL := s.cl.InferenceBase
	if baseURL == "" {
		baseURL = "https://autoglm-api.autoglm.ai"
	}
	body := `{"stream":false,"messages":[{"role":"user","content":"hi"}],"model":"x"}`
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST",
		baseURL+"/autoclaw-proxy/proxy/autoclaw/v1/chat/completions",
		strings.NewReader(body))
	if err != nil {
		return true
	}
	hdrs := s.cl.InferenceHeader(acct, route)
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.cl.Do(req)
	if err != nil {
		return true // jaringan gagal — jangan buang model
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode == 200:
		return true
	case resp.StatusCode == 400 && strings.Contains(string(b), "非法模型"):
		return false
	default:
		return true // 401/403/500 — model dikenali, masalah bukan di model
	}
}

// probeAccount mengambil access token akun aktif pertama untuk probe.
func (s *Server) probeAccount() (string, bool) {
	accounts, _ := db.ListAccounts()
	for _, a := range accounts {
		if a.Active && a.AccessToken != "" {
			return a.AccessToken, true
		}
	}
	return "", false
}

// handleHealth menangani /healthz.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	accounts, _ := db.ListAccounts()
	activeCount := 0
	for _, a := range accounts {
		if a.Active {
			activeCount++
		}
	}
	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"status":   "healthy",
		"accounts": len(accounts),
		"active":   activeCount,
		"time":     time.Now().Format(time.RFC3339),
	})
}

// refreshToken mencoba memperbarui token akun.
func (s *Server) refreshToken(acct *db.Account) (bool, error) {
	if acct.RefreshToken == "" {
		return false, fmt.Errorf("no refresh token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := s.cl.Refresh(ctx, acct.RefreshToken)
	if err != nil {
		return false, err
	}
	if out.Code != 0 || out.Data == nil || out.Data.AccessToken == "" {
		msg := "refresh gagal"
		if out != nil {
			msg = out.Msg
		}
		return false, fmt.Errorf("%s", msg)
	}
	newRefresh := acct.RefreshToken
	if out.Data.RefreshToken != "" {
		newRefresh = out.Data.RefreshToken
	}
	acct.AccessToken = out.Data.AccessToken
	acct.RefreshToken = newRefresh
	_ = db.UpdateAccount(acct)
	log.Printf("[autoclawpi] akun #%d token diperbarui", acct.ID)
	return true, nil
}

// ── helpers ─────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    code,
		},
	})
}

func replaceModel(raw []byte, model string) ([]byte, error) {
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	data["model"] = model
	return json.Marshal(data)
}

// isWAFBlockOnly melaporkan apakah body hanya berisi WAF block (tanpa konten apapun).
// Deteksi berbasis konten, bukan panjang body.
func isWAFBlockOnly(body []byte) bool {
	if !bytes.Contains(body, []byte(`"message":"forbidden"`)) {
		return false
	}
	return !bytes.Contains(body, []byte(`"choices"`)) &&
		!bytes.Contains(body, []byte(`"delta"`)) &&
		!bytes.Contains(body, []byte(`"usage"`)) &&
		!bytes.Contains(body, []byte(`"id"`))
}

// stripWAFPrefixes membuang semua prefix WAF {"message":"forbidden"} dari body.
func stripWAFPrefixes(body []byte) []byte {
	for bytes.Contains(body, []byte(`"message":"forbidden"`)) {
		idx := bytes.Index(body, []byte(`"message":"forbidden"`))
		next := bytes.Index(body[idx+1:], []byte(`{`))
		if next < 0 {
			break
		}
		body = body[idx+next+1:]
	}
	return body
}

// hasStreamStart melaporkan apakah buffer sudah berisi awal konten stream valid.
func hasStreamStart(b []byte) bool {
	return bytes.Contains(b, []byte("data:")) ||
		bytes.Contains(b, []byte(`"choices"`)) ||
		bytes.Contains(b, []byte(`"delta"`)) ||
		bytes.Contains(b, []byte(`"usage"`))
}

// hasContentMarkers melaporkan apakah body mengandung penanda konten chat
// completion valid (bukan error JSON murni). Dipakai untuk membedakan
// "WAF prefix + konten valid" vs "error JSON murni" pada response 403.
func hasContentMarkers(b []byte) bool {
	return bytes.Contains(b, []byte(`"choices"`)) ||
		bytes.Contains(b, []byte(`"delta"`)) ||
		bytes.Contains(b, []byte(`"usage"`))
}

// logUsageStream mencatat token usage dari response streaming (SSE).
// Upstream biasanya mengirim usage pada chunk terakhir.
func logUsageStream(acctID int64, model string, buf []byte) {
	entry := db.LogEntry{
		AccountID: acctID,
		Model:     model,
		Status:    "success",
	}
	for _, line := range bytes.Split(buf, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var data struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(payload, &data); err != nil {
			continue
		}
		if data.Usage != nil {
			entry.PromptTokens = data.Usage.PromptTokens
			entry.CompletionTokens = data.Usage.CompletionTokens
			entry.TotalTokens = data.Usage.TotalTokens
		}
		if data.Error != nil {
			entry.Status = "error"
			entry.Error = data.Error.Message
		}
	}
	entry.Cost = float64(entry.TotalTokens) * 0.001 / 1000.0
	_, _ = db.AddLog(&entry)
}

func truncateResp(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
