// Command mc-web serves the mc.danwolf.net landing page: how to connect, a link
// to the live BlueMap, the server rules, and a setup guide written for the
// parents of the 7 to 13 year old players. Everything (template and the
// version-select screenshot) is embedded, so the binary is the whole site.
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

//go:embed templates/index.html.tmpl
var indexSrc string

//go:embed assets
var assetsFS embed.FS

// pageData is the template context. Keep the dynamic, version-coupled values
// here so the guide and the connect card never drift from each other.
type pageData struct {
	PackVersion     string
	ServerAddress   string
	FallbackAddress string
	MapPath         string
	PortalPath      string
	Year            int
}

func main() {
	listen := flag.String("listen", ":8080", "address to listen on")
	packVersion := flag.String("pack-version", "7.1", "ATM10 pack version the server currently runs")
	serverAddr := flag.String("server-address", "mc.danwolf.net", "address players enter in the Minecraft launcher")
	fallbackAddr := flag.String("fallback-address", "game.danwolf.net:25999", "explicit fallback for launchers that ignore SRV")
	mapPath := flag.String("map-path", "/map/", "path the live BlueMap is served under (trailing slash matters)")
	portalPath := flag.String("portal-path", "/portal/", "path the member portal (sign in, invites) is served under")
	flag.Parse()

	data := pageData{
		PackVersion:     *packVersion,
		ServerAddress:   *serverAddr,
		FallbackAddress: *fallbackAddr,
		MapPath:         *mapPath,
		PortalPath:      *portalPath,
		Year:            time.Now().Year(),
	}

	// Render once at startup; the page is static for the process lifetime.
	tmpl := template.Must(template.New("index").Parse(indexSrc))
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		log.Fatalf("render index: %v", err)
	}
	page := []byte(buf.String())

	mux := http.NewServeMux()

	// Static assets (the screenshot). The embed FS already roots paths at
	// "assets/", so the /assets/ URL prefix maps straight through.
	assets := http.FileServer(http.FS(assetsFS))
	mux.Handle("/assets/", cacheControl(assets))

	// Infra health check, deliberately unstyled and dependency-free.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	})

	// Landing page. Only the exact root renders the page; anything else under
	// mc-web's / route is a genuine 404 rather than a soft catch-all.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("mc-web listening on %s (pack %s)", *listen, data.PackVersion)
	log.Fatal(srv.ListenAndServe())
}

func cacheControl(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}
