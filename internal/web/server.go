package web

import (
	"crypto/subtle"
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
	mux.HandleFunc("/keys/add", ws.basicAuth(ws.handleKeysAdd))
	mux.HandleFunc("/keys/delete", ws.basicAuth(ws.handleKeysDelete))
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
        setInterval(() => {
            const textarea = document.getElementById('keys-textarea');
            if (!textarea || (document.activeElement !== textarea && textarea.value.trim() === '')) {
                window.location.reload();
            }
        }, 10000);
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
                    Обновлено: <strong class="text-white">{{.RefreshedAt}}</strong> (авто-обновление 10с)
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
                    <h3 class="text-2xl font-bold text-white mt-1">{{.GeneralStats.TotalRequests}}</h3>
                    <p class="text-xs text-slate-400 mt-1">Сегодня: <strong class="text-emerald-400">{{.GeneralStats.TodayRequests}}</strong></p>
                </div>
                <div class="text-4xl">⚡</div>
            </div>

            <!-- Active Keys Card -->
            <div class="bg-slate-800 p-5 rounded-xl border border-slate-700 shadow-sm flex items-center justify-between">
                <div>
                    <p class="text-xs font-semibold text-slate-400 uppercase tracking-wider">Активные ключи</p>
                    <h3 class="text-2xl font-bold text-emerald-400 mt-1">{{.GeneralStats.ActiveKeys}} / {{.GeneralStats.TotalKeys}}</h3>
                    <div class="w-24 bg-slate-700 h-1.5 rounded-full mt-2 overflow-hidden">
                        <div class="bg-emerald-400 h-full" style="width: {{percentage (int64 .GeneralStats.ActiveKeys) (int64 .GeneralStats.TotalKeys)}}%"></div>
                    </div>
                </div>
                <div class="text-4xl">🔑</div>
            </div>

            <!-- Blocked / Cooldown Keys Card -->
            <div class="bg-slate-800 p-5 rounded-xl border border-slate-700 shadow-sm flex items-center justify-between">
                <div>
                    <p class="text-xs font-semibold text-slate-400 uppercase tracking-wider">В лимите / Cooldown</p>
                    <h3 class="text-2xl font-bold text-amber-500 mt-1">{{.GeneralStats.BlockedKeys}}</h3>
                    <div class="w-24 bg-slate-700 h-1.5 rounded-full mt-2 overflow-hidden">
                        <div class="bg-amber-500 h-full" style="width: {{percentage (int64 .GeneralStats.BlockedKeys) (int64 .GeneralStats.TotalKeys)}}%"></div>
                    </div>
                </div>
                <div class="text-4xl">⏳</div>
            </div>

            <!-- Invalid Keys Card -->
            <div class="bg-slate-800 p-5 rounded-xl border border-slate-700 shadow-sm flex items-center justify-between">
                <div>
                    <p class="text-xs font-semibold text-slate-400 uppercase tracking-wider">Невалидные / Ошибки</p>
                    <h3 class="text-2xl font-bold text-rose-500 mt-1">{{.GeneralStats.InvalidKeys}}</h3>
                    <p class="text-xs text-slate-400 mt-1">Непроверенные: <strong class="text-blue-400">{{.GeneralStats.UncheckedKeys}}</strong></p>
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
                        <tbody class="divide-y divide-slate-700">
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
            <div class="p-4 bg-slate-850 border-b border-slate-700 flex flex-col sm:flex-row sm:items-center justify-between gap-2">
                <div>
                    <h2 class="font-bold text-white flex items-center gap-2">
                        <span>🔑</span> Детализация API ключей (Аккаунтов)
                    </h2>
                    <p class="text-xs text-slate-400">Наглядный мониторинг лимитов и ротации по 1000+ ключам</p>
                </div>
                <div class="flex items-center gap-2 text-xs">
                    <span class="inline-flex items-center gap-1.5 px-2 py-1 rounded bg-emerald-500/10 border border-emerald-500/20 text-emerald-400"><span class="w-1.5 h-1.5 rounded-full bg-emerald-400"></span> Active</span>
                    <span class="inline-flex items-center gap-1.5 px-2 py-1 rounded bg-amber-500/10 border border-amber-500/20 text-amber-400"><span class="w-1.5 h-1.5 rounded-full bg-amber-400"></span> Limit/Cooldown</span>
                    <span class="inline-flex items-center gap-1.5 px-2 py-1 rounded bg-rose-500/10 border border-rose-500/20 text-rose-400"><span class="w-1.5 h-1.5 rounded-full bg-rose-400"></span> Exhausted</span>
                    <span class="inline-flex items-center gap-1.5 px-2 py-1 rounded bg-slate-500/15 border border-slate-500/20 text-slate-400"><span class="w-1.5 h-1.5 rounded-full bg-slate-400"></span> Invalid</span>
                </div>
            </div>
            <div class="overflow-x-auto max-h-[600px] overflow-y-auto">
                <table class="w-full text-sm text-left border-collapse">
                    <thead class="bg-slate-900 text-xs uppercase tracking-wider text-slate-400 border-b border-slate-700 sticky top-0 z-10">
                        <tr>
                            <th class="px-4 py-3">Ключ</th>
                            <th class="px-4 py-3">Статус</th>
                            <th class="px-4 py-3 text-center">Использовано сегодня</th>
                            <th class="px-4 py-3 text-center">Остаток лимита</th>
                            <th class="px-4 py-3 text-center">Всего запросов</th>
                            <th class="px-4 py-3 text-center">Процент ошибок</th>
                            <th class="px-4 py-3 text-center">Cooldown</th>
                            <th class="px-4 py-3 text-center">Last Used</th>
                            <th class="px-4 py-3 text-center">Действие</th>
                        </tr>
                    </thead>
                    <tbody class="divide-y divide-slate-700">
                        {{range .KeyStats}}
                        <tr class="hover:bg-slate-750 transition">
                            <td class="px-4 py-3 font-mono text-xs text-slate-300 font-medium">
                                <span title="{{.KeyHash}}">{{.MaskedKey}}</span>
                            </td>
                            <td class="px-4 py-3 text-xs">
                                {{if eq .Status "active"}}
                                <span class="inline-flex items-center px-2 py-1 rounded-md font-semibold bg-emerald-500/10 text-emerald-400 border border-emerald-500/20">ACTIVE</span>
                                {{else if eq .Status "rate_limited"}}
                                <span class="inline-flex items-center px-2 py-1 rounded-md font-semibold bg-amber-500/10 text-amber-400 border border-amber-500/20">COOLDOWN</span>
                                {{else if eq .Status "day_exhausted"}}
                                <span class="inline-flex items-center px-2 py-1 rounded-md font-semibold bg-rose-500/10 text-rose-400 border border-rose-500/20">EXHAUSTED</span>
                                {{else if eq .Status "invalid"}}
                                <span class="inline-flex items-center px-2 py-1 rounded-md font-semibold bg-slate-700/30 text-slate-500 border border-slate-700/50">INVALID</span>
                                {{else}}
                                <span class="inline-flex items-center px-2 py-1 rounded-md font-semibold bg-blue-500/10 text-blue-400 border border-blue-500/20">UNCHECKED</span>
                                {{end}}
                            </td>
                            <td class="px-4 py-3 text-center font-mono font-medium text-white">{{.TodayUsage}}</td>
                            <td class="px-4 py-3 text-center font-mono">
                                {{if le .Limit 0}}
                                <span class="text-slate-500">unknown</span>
                                {{else}}
                                <span class="{{if le .Limit 10}}text-rose-400 font-semibold{{else}}text-slate-300{{end}}">{{.Limit}}</span>
                                {{end}}
                            </td>
                            <td class="px-4 py-3 text-center font-mono text-slate-400">{{.TotalRequests}}</td>
                            <td class="px-4 py-3 text-center text-xs">
                                {{if gt .TotalRequests 0}}
                                <span class="font-mono {{if gt (percentage .ErrorRequests .TotalRequests) 10.0}}text-rose-400 font-semibold{{else}}text-slate-400{{end}}">
                                    {{printf "%.1f" (percentage .ErrorRequests .TotalRequests)}}%
                                </span>
                                {{else}}
                                <span class="text-slate-500">-</span>
                                {{end}}
                            </td>
                            <td class="px-4 py-3 text-center text-xs font-mono text-amber-400">
                                {{cooldownLeft .CooldownUntil}}
                            </td>
                             <td class="px-4 py-3 text-center text-xs text-slate-400">
                                {{formatTime .CooldownUntil}}
                             </td>
                             <td class="px-4 py-3 text-center">
                                 <form action="/keys/delete" method="POST" onsubmit="return confirm('Вы уверены, что хотите удалить этот ключ?');" class="inline">
                                     <input type="hidden" name="hash" value="{{.KeyHash}}">
                                     <button type="submit" class="text-rose-500 hover:text-rose-400 font-bold px-2 py-1 hover:bg-rose-500/10 rounded transition text-xs">🗑️ Удалить</button>
                                 </form>
                             </td>
                        </tr>
                        {{else}}
                        <tr>
                            <td colspan="9" class="px-4 py-8 text-center text-slate-400">Нет загруженных ключей в пуле.</td>
                        </tr>
                        {{end}}
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
