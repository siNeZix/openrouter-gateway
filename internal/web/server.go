package web

import (
	"crypto/subtle"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"openrouter-gateway/internal/config"
	"openrouter-gateway/internal/keys"
	"openrouter-gateway/internal/models"
	"openrouter-gateway/internal/store"
)

type WebServer struct {
	cfg        *config.Config
	store      *store.Store
	rankingMgr *models.RankingManager
	pool       *keys.KeyPool
}

type DashboardData struct {
	GeneralStats *store.GeneralStats
	ModelStats   []store.ModelStats
	KeyStats     []store.KeyUsageStats
	TopModels    []store.DBModel
	UpdateTime   string
	RefreshedAt  string
	Token        string
}

func NewWebServer(cfg *config.Config, s *store.Store, rm *models.RankingManager, pool *keys.KeyPool) *WebServer {
	return &WebServer{
		cfg:        cfg,
		store:      s,
		rankingMgr: rm,
		pool:       pool,
	}
}

func (ws *WebServer) Start(mux *http.ServeMux) {
	mux.HandleFunc("/", ws.basicAuth(ws.handleDashboard))
	mux.HandleFunc("/api/stats", ws.basicAuth(ws.handleAPIStats))
	mux.HandleFunc("/keys/add", ws.basicAuth(ws.handleKeysAdd))
	mux.HandleFunc("/keys/delete", ws.basicAuth(ws.handleKeysDelete))
	mux.HandleFunc("/keys/bulk", ws.basicAuth(ws.handleKeysBulk))
}

func (ws *WebServer) basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// If credentials are empty, bypass auth
		if ws.cfg.WebUsername == "" && ws.cfg.WebPassword == "" {
			next.ServeHTTP(w, r)
			return
		}

		user, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(ws.cfg.WebUsername)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(ws.cfg.WebPassword)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="OpenRouter Gateway"`)
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("Unauthorized\n"))
			return
		}

		next.ServeHTTP(w, r)
	}
}

func (ws *WebServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	general, err := ws.store.GetGeneralStats()
	if err != nil {
		log.Printf("Failed to get general stats: %v", err)
		http.Error(w, "Database error loading stats", http.StatusInternalServerError)
		return
	}

	modelsStats, err := ws.store.GetModelStats()
	if err != nil {
		log.Printf("Failed to get model stats: %v", err)
		http.Error(w, "Database error loading model stats", http.StatusInternalServerError)
		return
	}

	keyStats, err := ws.store.GetKeyUsageStats()
	if err != nil {
		log.Printf("Failed to get key stats: %v", err)
		http.Error(w, "Database error loading key stats", http.StatusInternalServerError)
		return
	}

	topModels := ws.rankingMgr.GetTopModels()

	data := DashboardData{
		GeneralStats: general,
		ModelStats:   modelsStats,
		KeyStats:     keyStats,
		TopModels:    topModels,
		RefreshedAt:  time.Now().Format("15:04:05 (02.01.2006)"),
		Token:        ws.cfg.GatewayToken,
	}

	tmpl, err := template.New("dashboard").Funcs(template.FuncMap{
		"percentage": func(part, total int64) float64 {
			if total == 0 {
				return 0
			}
			return (float64(part) / float64(total)) * 100
		},
		"percentageInt": func(part, total int) float64 {
			if total == 0 {
				return 0
			}
			return (float64(part) / float64(total)) * 100
		},
		"cooldownLeft": func(t time.Time) string {
			if t.Before(time.Now()) {
				return ""
			}
			return time.Until(t).Truncate(time.Second).String()
		},
		"formatTime": func(t time.Time) string {
			if t.IsZero() || t.Unix() <= 0 {
				return "never"
			}
			return t.Format("15:04:05")
		},
		"truncateModel": func(m string) string {
			parts := strings.Split(m, "/")
			if len(parts) > 1 {
				return parts[len(parts)-1]
			}
			return m
		},
		"add": func(x, y int) int {
			return x + y
		},
		"divInt": func(x, y int64) int64 {
			if y == 0 {
				return 0
			}
			return x / y
		},
	}).Parse(dashboardTemplate)

	if err != nil {
		log.Printf("Failed to parse html template: %v", err)
		http.Error(w, "Template error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Failed to render dashboard: %v", err)
	}
}

func (ws *WebServer) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	general, err := ws.store.GetGeneralStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	modelsStats, err := ws.store.GetModelStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	keyStats, err := ws.store.GetKeyUsageStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	topModels := ws.rankingMgr.GetTopModels()

	// Format cooldown durations for JSON response
	type CustomKeyStats struct {
		MaskedKey     string `json:"masked_key"`
		KeyHash       string `json:"key_hash"`
		Status        string `json:"status"`
		TodayUsage    int64  `json:"today_usage"`
		Limit         int64  `json:"limit"`
		TotalRequests int64  `json:"total_requests"`
		ErrorRequests int64  `json:"error_requests"`
		CooldownLeft  string `json:"cooldown_left"`
		FormattedLast string `json:"formatted_last"`
	}

	customKeys := make([]CustomKeyStats, len(keyStats))
	for i, k := range keyStats {
		left := ""
		if k.CooldownUntil.After(time.Now()) {
			left = time.Until(k.CooldownUntil).Truncate(time.Second).String()
		}
		formattedLast := "never"
		if !k.CooldownUntil.IsZero() && k.CooldownUntil.Unix() > 0 {
			formattedLast = k.CooldownUntil.Format("15:04:05")
		}
		customKeys[i] = CustomKeyStats{
			MaskedKey:     k.MaskedKey,
			KeyHash:       k.KeyHash,
			Status:        k.Status,
			TodayUsage:    k.TodayUsage,
			Limit:         k.Limit,
			TotalRequests: k.TotalRequests,
			ErrorRequests: k.ErrorRequests,
			CooldownLeft:  left,
			FormattedLast: formattedLast,
		}
	}

	type CustomGeneralStats struct {
		TotalRequests int64 `json:"total_requests"`
		TodayRequests int64 `json:"today_requests"`
		ActiveKeys    int   `json:"active_keys"`
		BlockedKeys   int   `json:"blocked_keys"`
		InvalidKeys   int   `json:"invalid_keys"`
		UncheckedKeys int   `json:"unchecked_keys"`
		TotalKeys     int   `json:"total_keys"`
	}

	customGeneral := CustomGeneralStats{
		TotalRequests: general.TotalRequests,
		TodayRequests: general.TodayRequests,
		ActiveKeys:    general.ActiveKeys,
		BlockedKeys:   general.BlockedKeys,
		InvalidKeys:   general.InvalidKeys,
		UncheckedKeys: general.UncheckedKeys,
		TotalKeys:     general.TotalKeys,
	}

	type CustomModelStats struct {
		Model         string `json:"model"`
		TotalRequests int64  `json:"total_requests"`
		AvgLatencyMs  int64  `json:"avg_latency_ms"`
		TotalTokens   int64  `json:"total_tokens"`
	}

	customModels := make([]CustomModelStats, len(modelsStats))
	for i, m := range modelsStats {
		customModels[i] = CustomModelStats{
			Model:         m.Model,
			TotalRequests: m.TotalRequests,
			AvgLatencyMs:  m.AvgLatencyMs,
			TotalTokens:   m.TotalTokens,
		}
	}

	res := map[string]interface{}{
		"general":      customGeneral,
		"models":       customModels,
		"keys":         customKeys,
		"top_models":   topModels,
		"refreshed_at": time.Now().Format("15:04:05 (02.01.2006)"),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (ws *WebServer) handleKeysAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	rawKeysText := r.FormValue("keys")
	var rawKeys []string
	lines := strings.Split(rawKeysText, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		rawKeys = append(rawKeys, line)
	}

	if len(rawKeys) > 0 {
		added, err := ws.pool.AddKeys(rawKeys)
		if err != nil {
			log.Printf("Failed to add keys: %v", err)
			http.Error(w, "Failed to add keys to database", http.StatusInternalServerError)
			return
		}
		log.Printf("Added %d new keys via Web GUI", added)
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (ws *WebServer) handleKeysDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	hash := r.FormValue("hash")
	if hash == "" {
		http.Error(w, "Missing key hash", http.StatusBadRequest)
		return
	}

	if err := ws.pool.RemoveKey(hash); err != nil {
		log.Printf("Failed to delete key %s: %v", hash, err)
		http.Error(w, "Failed to delete key from database", http.StatusInternalServerError)
		return
	}

	log.Printf("Deleted key hash %s via Web GUI", hash)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (ws *WebServer) handleKeysBulk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// We can receive multiple form parameters, or a single comma-separated string
	rawHashes := r.Form["hashes"]
	if len(rawHashes) == 1 && strings.Contains(rawHashes[0], ",") {
		rawHashes = strings.Split(rawHashes[0], ",")
	}

	var hashes []string
	for _, h := range rawHashes {
		h = strings.TrimSpace(h)
		if h != "" {
			hashes = append(hashes, h)
		}
	}

	action := r.FormValue("action")
	if len(hashes) == 0 || action == "" {
		http.Error(w, "Missing hashes or action", http.StatusBadRequest)
		return
	}

	var err error
	switch action {
	case "delete":
		err = ws.pool.RemoveKeys(hashes)
		log.Printf("Bulk deleted %d keys via Web GUI", len(hashes))
	case "enable":
		err = ws.pool.UpdateKeysStatus(hashes, "unchecked")
		log.Printf("Bulk enabled %d keys via Web GUI", len(hashes))
	case "disable":
		err = ws.pool.UpdateKeysStatus(hashes, "disabled")
		log.Printf("Bulk disabled %d keys via Web GUI", len(hashes))
	default:
		http.Error(w, "Unknown action", http.StatusBadRequest)
		return
	}

	if err != nil {
		log.Printf("Failed bulk operation (%s) on keys: %v", action, err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	// Support both AJAX/fetch and standard form submission redirects
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" || strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Inlined Tailwind Dashboard Template
const dashboardTemplate = `
<!DOCTYPE html>
<html lang="ru" class="h-full bg-slate-900">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>OpenRouter Free Gateway Dashboard</title>
    <script src="https://cdn.tailwindcss.com"></script>
    <style>
        @import url('https://fonts.googleapis.com/css2?family=Inter:wght@300;400;500;600;700&display=swap');
        body { font-family: 'Inter', sans-serif; }
    </style>
    <script>
        // Global State
        let allKeysData = [
            {{range .KeyStats}}
            {
                masked_key: "{{.MaskedKey}}",
                key_hash: "{{.KeyHash}}",
                status: "{{.Status}}",
                today_usage: {{.TodayUsage}},
                limit: {{.Limit}},
                total_requests: {{.TotalRequests}},
                error_requests: {{.ErrorRequests}},
                cooldown_left: "{{cooldownLeft .CooldownUntil}}",
                formatted_last: "{{formatTime .CooldownUntil}}"
            },
            {{end}}
        ];

        let sortCol = 'usage';
        let sortOrder = 'desc';
        let filterStatus = 'all';
        let searchQuery = '';
        const checkedHashes = new Set();

        async function updateStats() {
            try {
                const textarea = document.getElementById('keys-textarea');
                if (textarea && document.activeElement === textarea && textarea.value.trim() !== '') {
                    return; // Don't interrupt when typing keys
                }

                const res = await fetch('/api/stats');
                if (!res.ok) return;
                const data = await res.json();

                // Update updated time
                const refTime = document.getElementById('refreshed-at');
                if (refTime) refTime.textContent = data.refreshed_at;

                // Update general stats
                const totalReqs = document.getElementById('total-requests');
                if (totalReqs) totalReqs.textContent = data.general.total_requests;
                const todayReqs = document.getElementById('today-requests');
                if (todayReqs) todayReqs.textContent = data.general.today_requests;

                const activeKeys = document.getElementById('active-keys');
                if (activeKeys) activeKeys.textContent = data.general.active_keys + ' / ' + data.general.total_keys;
                const activeBar = document.getElementById('active-bar');
                if (activeBar) activeBar.style.width = (data.general.total_keys > 0 ? (data.general.active_keys / data.general.total_keys * 100) : 0) + '%';

                const blockedKeys = document.getElementById('blocked-keys');
                if (blockedKeys) blockedKeys.textContent = data.general.blocked_keys;
                const blockedBar = document.getElementById('blocked-bar');
                if (blockedBar) blockedBar.style.width = (data.general.total_keys > 0 ? (data.general.blocked_keys / data.general.total_keys * 100) : 0) + '%';

                const invalidKeys = document.getElementById('invalid-keys');
                if (invalidKeys) invalidKeys.textContent = data.general.invalid_keys;
                const uncheckedKeys = document.getElementById('unchecked-keys');
                if (uncheckedKeys) uncheckedKeys.textContent = data.general.unchecked_keys;

                // Update models usage stats table
                const modelBody = document.getElementById('model-stats-body');
                if (modelBody) {
                    if (data.models && data.models.length > 0) {
                        modelBody.innerHTML = data.models.map(m => {
                            const modelShort = m.model.split('/').pop() || m.model;
                            return "" +
                               "<tr class=\"hover:bg-slate-750 transition\">" +
                                   "<td class=\"px-4 py-3 font-semibold text-white\">" +
                                       modelShort +
                                       "<span class=\"block text-[10px] font-mono text-slate-500 font-normal mt-0.5\">" + m.model + "</span>" +
                                   "</td>" +
                                   "<td class=\"px-4 py-3 text-center text-slate-300 font-mono font-medium\">" + m.total_requests + "</td>" +
                                   "<td class=\"px-4 py-3 text-center text-amber-400 font-mono\">" + m.avg_latency_ms + " ms</td>" +
                                   "<td class=\"px-4 py-3 text-center text-slate-400 font-mono\">" + m.total_tokens + "</td>" +
                               "</tr>";
                        }).join('');
                    } else {
                        modelBody.innerHTML = "" +
                            "<tr>" +
                                "<td colspan=\"4\" class=\"px-4 py-8 text-center text-slate-400\">Лог запросов пуст. Сделайте первый запрос через прокси!</td>" +
                            "</tr>";
                    }
                }

                // Update in-memory keys
                allKeysData = data.keys || [];
                // Re-render table
                renderKeysTable();

            } catch (err) {
                console.error('Failed to auto update stats:', err);
            }
        }

        function renderKeysTable() {
            const keyBody = document.getElementById('key-stats-body');
            if (!keyBody) return;

            // 1. Filter
            let filtered = allKeysData.filter(k => {
                // Search query
                if (searchQuery) {
                    const q = searchQuery.toLowerCase();
                    if (!k.masked_key.toLowerCase().includes(q) && !k.key_hash.toLowerCase().includes(q)) {
                        return false;
                    }
                }
                // Status
                if (filterStatus === 'all') return true;
                if (filterStatus === 'active') return k.status === 'active';
                if (filterStatus === 'cooldown') return k.status === 'rate_limited';
                if (filterStatus === 'exhausted') return k.status === 'day_exhausted';
                if (filterStatus === 'invalid') return k.status === 'invalid';
                if (filterStatus === 'unchecked') return k.status === 'unchecked';
                if (filterStatus === 'disabled') return k.status === 'disabled';
                return true;
            });

            // 2. Sort
            filtered.sort((a, b) => {
                let valA, valB;
                switch(sortCol) {
                    case 'key':
                        valA = a.masked_key;
                        valB = b.masked_key;
                        break;
                    case 'status':
                        valA = a.status;
                        valB = b.status;
                        break;
                    case 'usage':
                        valA = Number(a.today_usage);
                        valB = Number(b.today_usage);
                        break;
                    case 'limit':
                        valA = Number(a.limit);
                        valB = Number(b.limit);
                        break;
                    case 'requests':
                        valA = Number(a.total_requests);
                        valB = Number(b.total_requests);
                        break;
                    case 'errors':
                        valA = a.total_requests > 0 ? (a.error_requests / a.total_requests) : -1;
                        valB = b.total_requests > 0 ? (b.error_requests / b.total_requests) : -1;
                        break;
                    case 'cooldown':
                        valA = a.cooldown_left || '';
                        valB = b.cooldown_left || '';
                        break;
                    case 'last_used':
                        valA = a.formatted_last || '';
                        valB = b.formatted_last || '';
                        break;
                    default:
                        valA = Number(a.today_usage);
                        valB = Number(b.today_usage);
                }

                if (valA < valB) return sortOrder === 'asc' ? -1 : 1;
                if (valA > valB) return sortOrder === 'asc' ? 1 : -1;
                return 0;
            });

            // Clean up any stale checked hashes that no longer exist
            const existingHashes = new Set(allKeysData.map(k => k.key_hash));
            for (let h of checkedHashes) {
                if (!existingHashes.has(h)) checkedHashes.delete(h);
            }

            // 3. HTML Render
            if (filtered.length > 0) {
                keyBody.innerHTML = filtered.map(k => {
                    let statusBadge = '';
                    if (k.status === 'active') {
                        statusBadge = '<span class="inline-flex items-center px-2 py-1 rounded-md font-semibold bg-emerald-500/10 text-emerald-400 border border-emerald-500/20">ACTIVE</span>';
                    } else if (k.status === 'rate_limited') {
                        statusBadge = '<span class="inline-flex items-center px-2 py-1 rounded-md font-semibold bg-amber-500/10 text-amber-400 border border-amber-500/20">COOLDOWN</span>';
                    } else if (k.status === 'day_exhausted') {
                        statusBadge = '<span class="inline-flex items-center px-2 py-1 rounded-md font-semibold bg-rose-500/10 text-rose-400 border border-rose-500/20">EXHAUSTED</span>';
                    } else if (k.status === 'invalid') {
                        statusBadge = '<span class="inline-flex items-center px-2 py-1 rounded-md font-semibold bg-slate-700/30 text-slate-500 border border-slate-700/50">INVALID</span>';
                    } else if (k.status === 'disabled') {
                        statusBadge = '<span class="inline-flex items-center px-2 py-1 rounded-md font-semibold bg-zinc-700/30 text-zinc-500 border border-zinc-700/50">DISABLED</span>';
                    } else {
                        statusBadge = '<span class="inline-flex items-center px-2 py-1 rounded-md font-semibold bg-blue-500/10 text-blue-400 border border-blue-500/20">UNCHECKED</span>';
                    }

                    const limitText = k.limit <= 0 ? '<span class="text-slate-500">unknown</span>' : 
                        ('<span class="' + (k.limit <= 10 ? 'text-rose-400 font-semibold' : 'text-slate-300') + '">' + k.limit + '</span>');

                    const errPercent = k.total_requests > 0 ? (k.error_requests / k.total_requests * 100) : 0;
                    const errText = k.total_requests > 0 ? 
                        ('<span class="font-mono ' + (errPercent > 10.0 ? 'text-rose-400 font-semibold' : 'text-slate-400') + '">' + errPercent.toFixed(1) + '%</span>') : 
                        '<span class="text-slate-500">-</span>';

                    const isChecked = checkedHashes.has(k.key_hash) ? 'checked' : '';

                    // Switch toggler
                    const toggleBtn = k.status === 'disabled' ? 
                        '<button onclick="performAction(\'enable\', [\'' + k.key_hash + '\'])" class="bg-emerald-500/10 text-emerald-400 hover:bg-emerald-500/20 border border-emerald-500/20 rounded px-2 py-1 text-xs font-semibold transition" title="Включить ключ">🟢 Вкл</button>' :
                        '<button onclick="performAction(\'disable\', [\'' + k.key_hash + '\'])" class="bg-slate-700/50 text-slate-400 hover:bg-slate-700 hover:text-white border border-slate-600 rounded px-2 py-1 text-xs font-semibold transition" title="Выключить ключ">🔴 Выкл</button>';

                    return '<tr class="hover:bg-slate-750 transition ' + (k.status === 'disabled' ? 'opacity-50' : '') + '">' +
                            '<td class="px-4 py-3 text-center">' +
                                '<input type="checkbox" data-hash="' + k.key_hash + '" onchange="toggleKeySelect(this, \'' + k.key_hash + '\')" ' + isChecked + ' class="key-checkbox rounded bg-slate-900 border-slate-700 text-indigo-600 focus:ring-indigo-500 h-4 w-4">' +
                            '</td>' +
                            '<td class="px-4 py-3 font-mono text-xs text-slate-300 font-medium">' +
                                '<span title="' + k.key_hash + '">' + k.masked_key + '</span>' +
                            '</td>' +
                            '<td class="px-4 py-3 text-xs">' +
                                statusBadge +
                            '</td>' +
                            '<td class="px-4 py-3 text-center font-mono font-medium text-white">' + k.today_usage + '</td>' +
                            '<td class="px-4 py-3 text-center font-mono">' +
                                limitText +
                            '</td>' +
                            '<td class="px-4 py-3 text-center font-mono text-slate-400">' + k.total_requests + '</td>' +
                            '<td class="px-4 py-3 text-center text-xs">' +
                                errText +
                            '</td>' +
                            '<td class="px-4 py-3 text-center text-xs font-mono text-amber-400">' +
                                (k.cooldown_left || '') +
                            '</td>' +
                            '<td class="px-4 py-3 text-center text-xs text-slate-400">' +
                                (k.formatted_last || 'never') +
                            '</td>' +
                            '<td class="px-4 py-3 text-center">' +
                                '<div class="flex items-center justify-center gap-1.5">' +
                                    toggleBtn +
                                    '<button onclick="performAction(\'delete\', [\'' + k.key_hash + '\'])" class="text-rose-500 hover:text-rose-400 font-bold px-2 py-1 hover:bg-rose-500/10 rounded transition text-xs">🗑️  Удалить</button>' +
                                '</div>' +
                            '</td>' +
                        '</tr>';
                }).join('');
            } else {
                keyBody.innerHTML = '<tr><td colspan="10" class="px-4 py-8 text-center text-slate-400">Ключи не найдены по заданным фильтрам.</td></tr>';
            }

            updateSortIndicators();
            updateSelectAllCheckbox();
            updateBulkBar();
        }

        // Selection Handlers
        function toggleKeySelect(checkbox, hash) {
            if (checkbox.checked) {
                checkedHashes.add(hash);
            } else {
                checkedHashes.delete(hash);
            }
            updateSelectAllCheckbox();
            updateBulkBar();
        }

        function toggleSelectAll(masterCheckbox) {
            // Get currently rendered / filtered keys to select only those
            const visibleCheckboxes = document.querySelectorAll('.key-checkbox');
            visibleCheckboxes.forEach(cb => {
                const hash = cb.getAttribute('data-hash');
                cb.checked = masterCheckbox.checked;
                if (masterCheckbox.checked) {
                    checkedHashes.add(hash);
                } else {
                    checkedHashes.delete(hash);
                }
            });
            updateBulkBar();
        }

        function updateSelectAllCheckbox() {
            const master = document.getElementById('select-all-keys');
            if (!master) return;
            const visibleCheckboxes = document.querySelectorAll('.key-checkbox');
            if (visibleCheckboxes.length === 0) {
                master.checked = false;
                return;
            }
            let allChecked = true;
            visibleCheckboxes.forEach(cb => {
                if (!cb.checked) allChecked = false;
            });
            master.checked = allChecked;
        }

        function updateBulkBar() {
            const bar = document.getElementById('bulk-actions-bar');
            const countSpan = document.getElementById('selected-count');
            if (bar && countSpan) {
                const count = checkedHashes.size;
                countSpan.textContent = count;
                if (count > 0) {
                    bar.classList.remove('hidden');
                } else {
                    bar.classList.add('hidden');
                }
            }
        }

        // Sort & Filter Handlers
        function toggleSort(col) {
            if (sortCol === col) {
                sortOrder = sortOrder === 'asc' ? 'desc' : 'asc';
            } else {
                sortCol = col;
                sortOrder = 'desc'; // default to desc
            }
            renderKeysTable();
        }

        function setFilterStatus(status) {
            filterStatus = status;
            // Update active state in UI
            const statuses = ['all', 'active', 'cooldown', 'exhausted', 'invalid', 'unchecked', 'disabled'];
            statuses.forEach(s => {
                const btn = document.getElementById('filter-btn-' + s);
                if (btn) {
                    if (s === status) {
                        btn.className = "px-2.5 py-1.5 rounded-lg border font-semibold transition bg-indigo-600 border-indigo-500 text-white";
                    } else {
                        btn.className = "px-2.5 py-1.5 rounded-lg border font-semibold transition border-slate-700 text-slate-400 hover:text-white hover:border-slate-600";
                    }
                }
            });
            renderKeysTable();
        }

        function handleSearch(val) {
            searchQuery = val;
            renderKeysTable();
        }

        // Actions
        async function performAction(action, hashesArray) {
            if (hashesArray.length === 0) return;
            if (action === 'delete' && !confirm('Вы уверены, что хотите удалить ' + hashesArray.length + ' ключ(ей)?')) {
                return;
            }

            try {
                const formData = new URLSearchParams();
                hashesArray.forEach(h => formData.append('hashes', h));
                formData.append('action', action);

                const res = await fetch('/keys/bulk', {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/x-www-form-urlencoded',
                        'X-Requested-With': 'XMLHttpRequest'
                    },
                    body: formData.toString()
                });

                if (res.ok) {
                    // Success, remove performed hashes from the checked set
                    hashesArray.forEach(h => checkedHashes.delete(h));
                    await updateStats();
                } else {
                    alert('Ошибка при выполнении операции на сервере.');
                }
            } catch (err) {
                console.error('Action error:', err);
                alert('Сетевая ошибка при выполнении операции.');
            }
        }

        function bulkAction(action) {
            const hashes = Array.from(checkedHashes);
            performAction(action, hashes);
        }

        // Run on initial load
        window.addEventListener('DOMContentLoaded', () => {
            renderKeysTable();
            setInterval(updateStats, 5000);
        });
    </script>
</head>
<body class="h-full text-slate-100 flex flex-col">
    <!-- Header -->
    <header class="bg-slate-800 border-b border-slate-700 shadow-lg px-6 py-4">
        <div class="max-w-7xl mx-auto flex flex-col md:flex-row items-center justify-between gap-4">
            <div class="flex items-center gap-3">
                <span class="text-2xl">🌐</span>
                <div>
                    <h1 class="text-xl font-bold tracking-tight text-white">OpenRouter Free Gateway</h1>
                    <p class="text-xs text-slate-400">Умный прокси-ротатор • Стек Go & SQLite</p>
                </div>
            </div>
            <div class="flex flex-wrap items-center gap-4 text-sm">
                <div class="bg-slate-900 px-3 py-1.5 rounded-md border border-slate-700 text-xs">
                    <span class="text-slate-400">Gateway Token:</span>
                    <code class="text-emerald-400 font-mono ml-1">{{.Token}}</code>
                </div>
                <div class="bg-slate-900 px-3 py-1.5 rounded-md border border-slate-700 text-xs text-slate-400">
                    Обновлено: <strong id="refreshed-at" class="text-white">{{.RefreshedAt}}</strong> (авто-обновление 5с)
                </div>
            </div>
        </div>
    </header>

    <!-- Main Content -->
    <main class="flex-1 max-w-7xl w-full mx-auto p-6 space-y-6 overflow-y-auto">
        <!-- General Summary Cards -->
        <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
            <!-- Requests Card -->
            <div class="bg-slate-800 p-5 rounded-xl border border-slate-700 shadow-sm flex items-center justify-between">
                <div>
                    <p class="text-xs font-semibold text-slate-400 uppercase tracking-wider">Всего запросов</p>
                    <h3 id="total-requests" class="text-2xl font-bold text-white mt-1">{{.GeneralStats.TotalRequests}}</h3>
                    <p class="text-xs text-slate-400 mt-1">Сегодня: <strong id="today-requests" class="text-emerald-400">{{.GeneralStats.TodayRequests}}</strong></p>
                </div>
                <div class="text-4xl">⚡</div>
            </div>

            <!-- Active Keys Card -->
            <div class="bg-slate-800 p-5 rounded-xl border border-slate-700 shadow-sm flex items-center justify-between">
                <div>
                    <p class="text-xs font-semibold text-slate-400 uppercase tracking-wider">Активные ключи</p>
                    <h3 id="active-keys" class="text-2xl font-bold text-emerald-400 mt-1">{{.GeneralStats.ActiveKeys}} / {{.GeneralStats.TotalKeys}}</h3>
                    <div class="w-24 bg-slate-700 h-1.5 rounded-full mt-2 overflow-hidden">
                        <div id="active-bar" class="bg-emerald-400 h-full" style="width: {{percentageInt .GeneralStats.ActiveKeys .GeneralStats.TotalKeys}}%"></div>
                    </div>
                </div>
                <div class="text-4xl">🔑</div>
            </div>

            <!-- Blocked / Cooldown Keys Card -->
            <div class="bg-slate-800 p-5 rounded-xl border border-slate-700 shadow-sm flex items-center justify-between">
                <div>
                    <p class="text-xs font-semibold text-slate-400 uppercase tracking-wider">В лимите / Cooldown</p>
                    <h3 id="blocked-keys" class="text-2xl font-bold text-amber-500 mt-1">{{.GeneralStats.BlockedKeys}}</h3>
                    <div class="w-24 bg-slate-700 h-1.5 rounded-full mt-2 overflow-hidden">
                        <div id="blocked-bar" class="bg-amber-500 h-full" style="width: {{percentageInt .GeneralStats.BlockedKeys .GeneralStats.TotalKeys}}%"></div>
                    </div>
                </div>
                <div class="text-4xl">⏳</div>
            </div>

            <!-- Invalid Keys Card -->
            <div class="bg-slate-800 p-5 rounded-xl border border-slate-700 shadow-sm flex items-center justify-between">
                <div>
                    <p class="text-xs font-semibold text-slate-400 uppercase tracking-wider">Невалидные / Ошибки</p>
                    <h3 id="invalid-keys" class="text-2xl font-bold text-rose-500 mt-1">{{.GeneralStats.InvalidKeys}}</h3>
                    <p class="text-xs text-slate-400 mt-1">Непроверенные: <strong id="unchecked-keys" class="text-blue-400">{{.GeneralStats.UncheckedKeys}}</strong></p>
                </div>
                <div class="text-4xl">❌</div>
            </div>
        </div>

        <!-- Two Columns Layout: Models Top & Usage Stats -->
        <div class="grid grid-cols-1 lg:grid-cols-12 gap-6">
            <!-- Left Column: Shir-Man Top Free Models (4 cols) -->
            <section class="lg:col-span-5 bg-slate-800 rounded-xl border border-slate-700 overflow-hidden flex flex-col shadow-sm">
                <div class="p-4 bg-slate-850 border-b border-slate-700 flex justify-between items-center">
                    <h2 class="font-bold text-white flex items-center gap-2">
                        <span>🏆</span> Топ Free моделей (Shir-Man)
                    </h2>
                    <span class="text-xs bg-indigo-500/10 text-indigo-400 border border-indigo-500/20 px-2 py-0.5 rounded">Free Only</span>
                </div>
                <div class="p-4 flex-1 space-y-3 overflow-y-auto max-h-[450px]">
                    {{range $index, $m := .TopModels}}
                    <div class="bg-slate-900 border border-slate-700 rounded-lg p-3 flex items-center justify-between gap-2 hover:border-slate-600 transition">
                        <div class="flex items-center gap-3">
                            <span class="flex items-center justify-center w-7 h-7 rounded-full bg-slate-800 border border-slate-600 text-xs font-bold text-slate-300">
                                {{if eq $index 0}}🥇{{else if eq $index 1}}🥈{{else if eq $index 2}}🥉{{else}}{{$m.Rank}}{{end}}
                            </span>
                            <div>
                                <h4 class="text-sm font-semibold text-white flex items-center gap-1.5">
                                    {{$m.Name}}
                                    {{if lt $index 3}}
                                    <span class="text-[10px] uppercase font-bold tracking-wider text-emerald-400 bg-emerald-500/10 border border-emerald-500/20 px-1 py-0.5 rounded">
                                        top{{add $index 1}}
                                    </span>
                                    {{end}}
                                </h4>
                                <code class="text-xs font-mono text-slate-500">{{$m.ID}}</code>
                            </div>
                        </div>
                        <div class="text-right text-xs">
                            <span class="text-slate-400 block">Контекст</span>
                            <strong class="text-slate-300">{{divInt $m.ContextLength 1024}}K</strong>
                        </div>
                    </div>
                    {{else}}
                    <p class="text-sm text-slate-400 text-center py-6">Рейтинг моделей еще не загружен.</p>
                    {{end}}
                </div>
            </section>

            <!-- Right Column: Model Usage Statistics (7 cols) -->
            <section class="lg:col-span-7 bg-slate-800 rounded-xl border border-slate-700 overflow-hidden flex flex-col shadow-sm">
                <div class="p-4 bg-slate-850 border-b border-slate-700">
                    <h2 class="font-bold text-white flex items-center gap-2">
                        <span>📊</span> Статистика использования моделей
                    </h2>
                </div>
                <div class="overflow-x-auto flex-1 max-h-[450px]">
                    <table class="w-full text-sm text-left border-collapse">
                        <thead class="bg-slate-900 text-xs uppercase tracking-wider text-slate-400 border-b border-slate-700">
                            <tr>
                                <th class="px-4 py-3">Модель</th>
                                <th class="px-4 py-3 text-center">Запросы</th>
                                <th class="px-4 py-3 text-center">Ср. задержка</th>
                                <th class="px-4 py-3 text-center">Токены</th>
                            </tr>
                        </thead>
                        <tbody id="model-stats-body" class="divide-y divide-slate-700">
                            {{range .ModelStats}}
                            <tr class="hover:bg-slate-750 transition">
                                <td class="px-4 py-3 font-semibold text-white">
                                    {{truncateModel .Model}}
                                    <span class="block text-[10px] font-mono text-slate-500 font-normal mt-0.5">{{.Model}}</span>
                                </td>
                                <td class="px-4 py-3 text-center text-slate-300 font-mono font-medium">{{.TotalRequests}}</td>
                                <td class="px-4 py-3 text-center text-amber-400 font-mono">{{.AvgLatencyMs}} ms</td>
                                <td class="px-4 py-3 text-center text-slate-400 font-mono">{{.TotalTokens}}</td>
                            </tr>
                            {{else}}
                            <tr>
                                <td colspan="4" class="px-4 py-8 text-center text-slate-400">Лог запросов пуст. Сделайте первый запрос через прокси!</td>
                            </tr>
                            {{end}}
                        </tbody>
                    </table>
                </div>
            </section>
        </div>

        <!-- Add Keys Section -->
        <section class="bg-slate-800 rounded-xl border border-slate-700 overflow-hidden shadow-sm p-5">
            <h2 class="font-bold text-white flex items-center gap-2 mb-3">
                <span>➕</span> Добавить новые API ключи
            </h2>
            <form action="/keys/add" method="POST" class="space-y-3">
                <textarea id="keys-textarea" name="keys" rows="3" class="w-full bg-slate-900 border border-slate-700 rounded-lg p-3 text-sm font-mono focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500 text-slate-100 placeholder-slate-500" placeholder="Вставьте ключи, каждый с новой строки (пустые строки и комментарии # или // пропускаются)&#10;sk-or-v1-...&#10;sk-or-v1-..."></textarea>
                <div class="flex justify-end">
                    <button type="submit" class="bg-indigo-600 hover:bg-indigo-500 active:bg-indigo-700 text-white px-5 py-2 rounded-lg font-semibold text-sm transition">Добавить ключи</button>
                </div>
            </form>
        </section>

        <!-- Detailed Account Keys Status (Bottom Section) -->
        <section class="bg-slate-800 rounded-xl border border-slate-700 overflow-hidden shadow-sm flex flex-col">
            <div class="p-4 bg-slate-850 border-b border-slate-700 flex flex-col lg:flex-row lg:items-center justify-between gap-4">
                <div>
                    <h2 class="font-bold text-white flex items-center gap-2">
                        <span>🔑</span> Детализация API ключей (Аккаунтов)
                    </h2>
                    <p class="text-xs text-slate-400">Наглядный мониторинг лимитов и ротации по 1000+ ключам</p>
                </div>
                <div class="flex flex-wrap items-center gap-2 text-xs">
                    <span class="inline-flex items-center gap-1.5 px-2 py-1 rounded bg-emerald-500/10 border border-emerald-500/20 text-emerald-400"><span class="w-1.5 h-1.5 rounded-full bg-emerald-400"></span> Active</span>
                    <span class="inline-flex items-center gap-1.5 px-2 py-1 rounded bg-amber-500/10 border border-amber-500/20 text-amber-400"><span class="w-1.5 h-1.5 rounded-full bg-amber-400"></span> Limit/Cooldown</span>
                    <span class="inline-flex items-center gap-1.5 px-2 py-1 rounded bg-rose-500/10 border border-rose-500/20 text-rose-400"><span class="w-1.5 h-1.5 rounded-full bg-rose-400"></span> Exhausted</span>
                    <span class="inline-flex items-center gap-1.5 px-2 py-1 rounded bg-slate-500/15 border border-slate-500/20 text-slate-400"><span class="w-1.5 h-1.5 rounded-full bg-slate-400"></span> Invalid</span>
                    <span class="inline-flex items-center gap-1.5 px-2 py-1 rounded bg-zinc-500/15 border border-zinc-500/20 text-zinc-400"><span class="w-1.5 h-1.5 rounded-full bg-zinc-400"></span> Disabled</span>
                </div>
            </div>

            <!-- Bulk Actions Bar -->
            <div id="bulk-actions-bar" class="hidden bg-indigo-950/40 border-b border-slate-700 px-4 py-2.5 flex items-center justify-between gap-4 transition-all">
                <div class="flex items-center gap-2 text-xs text-slate-300">
                    <span class="font-bold text-indigo-400" id="selected-count">0</span> ключей выбрано
                </div>
                <div class="flex items-center gap-2">
                    <button onclick="bulkAction('enable')" class="bg-emerald-600 hover:bg-emerald-500 active:bg-emerald-700 text-white font-semibold px-2.5 py-1 rounded text-xs transition">🟢 Включить</button>
                    <button onclick="bulkAction('disable')" class="bg-slate-700 hover:bg-slate-600 active:bg-slate-800 text-white font-semibold px-2.5 py-1 rounded text-xs transition">🚫 Отключить</button>
                    <button onclick="bulkAction('delete')" class="bg-rose-600 hover:bg-rose-500 active:bg-rose-700 text-white font-semibold px-2.5 py-1 rounded text-xs transition">🗑️ Удалить</button>
                </div>
            </div>

            <!-- Filter and Search Bar -->
            <div class="p-4 bg-slate-850/50 border-b border-slate-700 flex flex-col md:flex-row items-center justify-between gap-4">
                <!-- Search Input -->
                <div class="relative w-full md:w-72">
                    <span class="absolute inset-y-0 left-0 flex items-center pl-3 text-slate-500">🔍</span>
                    <input type="text" id="search-input" oninput="handleSearch(this.value)" placeholder="Поиск по маске..." class="w-full pl-9 bg-slate-900 border border-slate-700 rounded-lg py-1.5 px-3 text-sm focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500 text-slate-100 placeholder-slate-500">
                </div>
                <!-- Status Filter Badges -->
                <div class="flex flex-wrap items-center gap-1.5 text-xs">
                    <button onclick="setFilterStatus('all')" id="filter-btn-all" class="px-2.5 py-1.5 rounded-lg border font-semibold transition bg-indigo-600 border-indigo-500 text-white">Все</button>
                    <button onclick="setFilterStatus('active')" id="filter-btn-active" class="px-2.5 py-1.5 rounded-lg border font-semibold transition border-slate-700 text-slate-400 hover:text-white hover:border-slate-600">Active</button>
                    <button onclick="setFilterStatus('cooldown')" id="filter-btn-cooldown" class="px-2.5 py-1.5 rounded-lg border font-semibold transition border-slate-700 text-slate-400 hover:text-white hover:border-slate-600">Cooldown</button>
                    <button onclick="setFilterStatus('exhausted')" id="filter-btn-exhausted" class="px-2.5 py-1.5 rounded-lg border font-semibold transition border-slate-700 text-slate-400 hover:text-white hover:border-slate-600">Exhausted</button>
                    <button onclick="setFilterStatus('invalid')" id="filter-btn-invalid" class="px-2.5 py-1.5 rounded-lg border font-semibold transition border-slate-700 text-slate-400 hover:text-white hover:border-slate-600">Invalid</button>
                    <button onclick="setFilterStatus('unchecked')" id="filter-btn-unchecked" class="px-2.5 py-1.5 rounded-lg border font-semibold transition border-slate-700 text-slate-400 hover:text-white hover:border-slate-600">Unchecked</button>
                    <button onclick="setFilterStatus('disabled')" id="filter-btn-disabled" class="px-2.5 py-1.5 rounded-lg border font-semibold transition border-slate-700 text-slate-400 hover:text-white hover:border-slate-600">Disabled</button>
                </div>
            </div>

            <div class="overflow-x-auto max-h-[600px] overflow-y-auto">
                <table class="w-full text-sm text-left border-collapse">
                    <thead class="bg-slate-900 text-xs uppercase tracking-wider text-slate-400 border-b border-slate-700 sticky top-0 z-10">
                        <tr>
                            <th class="px-4 py-3 text-center w-10">
                                <input type="checkbox" id="select-all-keys" onchange="toggleSelectAll(this)" class="rounded bg-slate-900 border-slate-700 text-indigo-600 focus:ring-indigo-500 h-4 w-4">
                            </th>
                            <th onclick="toggleSort('key')" class="px-4 py-3 cursor-pointer hover:bg-slate-850 select-none transition">Ключ <span id="sort-indicator-key"></span></th>
                            <th onclick="toggleSort('status')" class="px-4 py-3 cursor-pointer hover:bg-slate-850 select-none transition">Статус <span id="sort-indicator-status"></span></th>
                            <th onclick="toggleSort('usage')" class="px-4 py-3 text-center cursor-pointer hover:bg-slate-850 select-none transition">Использовано сегодня <span id="sort-indicator-usage"></span></th>
                            <th onclick="toggleSort('limit')" class="px-4 py-3 text-center cursor-pointer hover:bg-slate-850 select-none transition">Остаток лимита <span id="sort-indicator-limit"></span></th>
                            <th onclick="toggleSort('requests')" class="px-4 py-3 text-center cursor-pointer hover:bg-slate-850 select-none transition">Всего запросов <span id="sort-indicator-requests"></span></th>
                            <th onclick="toggleSort('errors')" class="px-4 py-3 text-center cursor-pointer hover:bg-slate-850 select-none transition">Процент ошибок <span id="sort-indicator-errors"></span></th>
                            <th onclick="toggleSort('cooldown')" class="px-4 py-3 text-center cursor-pointer hover:bg-slate-850 select-none transition">Cooldown <span id="sort-indicator-cooldown"></span></th>
                            <th onclick="toggleSort('last_used')" class="px-4 py-3 text-center cursor-pointer hover:bg-slate-850 select-none transition">Last Used <span id="sort-indicator-last_used"></span></th>
                            <th class="px-4 py-3 text-center">Действие</th>
                        </tr>
                    </thead>
                    <tbody id="key-stats-body" class="divide-y divide-slate-700">
                        <!-- Will be populated dynamically via renderKeysTable() on load and update -->
                    </tbody>
                </table>
            </div>
        </section>
    </main>

    <!-- Footer -->
    <footer class="bg-slate-800 border-t border-slate-700 text-center py-4 text-xs text-slate-400 mt-auto">
        <p>Разработано с заботой о лимитах • Использование свободных LLM без переплат</p>
    </footer>
</body>
</html>
`
