// Command mc-web serves the mc.danwolf.net public pages: the landing page (how
// to connect, a link to the live BlueMap, a live "who's online" widget, the
// rules, and a setup guide for the parents of the 7 to 13 year old players), a
// player tips page (getting started with heavily modded Minecraft and ATM10),
// and a parental-controls tips page. Everything (templates and the
// version-select screenshot) is embedded, so the binary is the whole site.
//
// The pages are static, but they call two same-origin portal endpoints from the
// browser: /portal/players for the live online list, and /portal/whoami so the
// "Sign in" nav link becomes the member's name once they are signed in.
//
// The pack version and addresses are flags with sane defaults so a pack upgrade
// is a one-line change here (and the screenshot swap), per the upgrade runbook.
package main

import (
	"embed"
	"flag"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"
)

//go:embed templates/*.tmpl
var templatesFS embed.FS

//go:embed assets
var assetsFS embed.FS

// pageData is the shared template context. Keep the dynamic, version-coupled
// values here so the guide, nav, and connect card never drift.
type pageData struct {
	PackVersion     string
	ServerAddress   string
	FallbackAddress string
	MapPath         string
	PortalPath      string
	ParentsPath     string
	TipsPath        string
	RulesPath       string
	CurseforgePath  string
	VanillaPath     string
	PlayersURL      string
	WhoamiURL       string
	MetricsURL      string
	Year            int
}

func main() {
	listen := flag.String("listen", ":8080", "address to listen on")
	packVersion := flag.String("pack-version", "7.1", "ATM10 pack version the server currently runs")
	serverAddr := flag.String("server-address", "mc.danwolf.net", "address players enter in the Minecraft launcher")
	fallbackAddr := flag.String("fallback-address", "game.danwolf.net:25999", "explicit fallback for launchers that ignore SRV")
	mapPath := flag.String("map-path", "/map/", "path the live BlueMap is served under (trailing slash matters)")
	portalPath := flag.String("portal-path", "/portal/", "path the member portal (sign in, invites) is served under")
	parentsPath := flag.String("parents-path", "/parents", "path of the parental-controls tips page")
	tipsPath := flag.String("tips-path", "/tips", "path of the ATM10 player tips page")
	rulesPath := flag.String("rules-path", "/rules", "path of the house rules page")
	curseforgePath := flag.String("curseforge-path", "/curseforge", "path of the CurseForge install guide")
	vanillaPath := flag.String("vanilla-path", "/vanilla", "path of the vanilla-launcher install guide")
	playersURL := flag.String("players-url", "/portal/players", "same-origin endpoint returning the online-players fragment")
	whoamiURL := flag.String("whoami-url", "/portal/whoami", "same-origin endpoint reporting the visitor's sign-in state")
	metricsURL := flag.String("metrics-url", "https://grafana.danwolf.net/d/mc-atm10", "Grafana dashboard, shown in the admin user-menu (tailnet-only)")
	flag.Parse()

	data := pageData{
		PackVersion:     *packVersion,
		ServerAddress:   *serverAddr,
		FallbackAddress: *fallbackAddr,
		MapPath:         *mapPath,
		PortalPath:      *portalPath,
		ParentsPath:     *parentsPath,
		TipsPath:        *tipsPath,
		RulesPath:       *rulesPath,
		CurseforgePath:  *curseforgePath,
		VanillaPath:     *vanillaPath,
		PlayersURL:      *playersURL,
		WhoamiURL:       *whoamiURL,
		MetricsURL:      *metricsURL,
		Year:            time.Now().Year(),
	}

	tmpl := template.Must(template.ParseFS(templatesFS, "templates/*.tmpl"))
	index := renderPage(tmpl, "index.html.tmpl", data)
	parents := renderPage(tmpl, "parents.html.tmpl", data)
	tips := renderPage(tmpl, "tips.html.tmpl", data)
	rules := renderPage(tmpl, "rules.html.tmpl", data)
	curseforge := renderPage(tmpl, "curseforge.html.tmpl", data)
	vanilla := renderPage(tmpl, "vanilla.html.tmpl", data)

	mux := http.NewServeMux()

	assets := http.FileServer(http.FS(assetsFS))
	mux.Handle("/assets/", cacheControl(assets))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	})

	mux.HandleFunc(strings.TrimRight(*parentsPath, "/"), servePage(parents))
	mux.HandleFunc(strings.TrimRight(*tipsPath, "/"), servePage(tips))
	mux.HandleFunc(strings.TrimRight(*rulesPath, "/"), servePage(rules))
	mux.HandleFunc(strings.TrimRight(*curseforgePath, "/"), servePage(curseforge))
	mux.HandleFunc(strings.TrimRight(*vanillaPath, "/"), servePage(vanilla))

	// Landing page. Only the exact root renders it; anything else under
	// mc-web's / route that is not a more specific match is a genuine 404.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		servePage(index)(w, r)
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("mc-web listening on %s (pack %s)", *listen, data.PackVersion)
	log.Fatal(srv.ListenAndServe())
}

// renderPage executes one template to bytes at startup; pages are static for
// the process lifetime.
func renderPage(tmpl *template.Template, name string, data pageData) []byte {
	var buf strings.Builder
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Fatalf("render %s: %v", name, err)
	}
	return []byte(buf.String())
}

func servePage(page []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	}
}

func cacheControl(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}
