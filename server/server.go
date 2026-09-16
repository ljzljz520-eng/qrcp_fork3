package server

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/claudiodangelis/qrcp/manifest"
	"github.com/claudiodangelis/qrcp/qr"

	"github.com/claudiodangelis/qrcp/config"
	"github.com/claudiodangelis/qrcp/pages"
	"github.com/claudiodangelis/qrcp/util"
	"gopkg.in/cheggaaa/pb.v1"
)

// tokenPlaceholder is replaced in the embedded transfer page with the
// session token. The token itself is lowercase hex, so the replacement is
// safe inside HTML and JavaScript contexts.
const tokenPlaceholder = "__QCTP_TOKEN__"

// Server is the server
type Server struct {
	BaseURL string
	// SendURL is the URL used to send the file
	SendURL string
	// ReceiveURL is the URL used to Receive the file
	ReceiveURL   string
	instance     *http.Server
	mux          *http.ServeMux
	outputDir    string
	stopChannel  chan bool
	shutdownOnce sync.Once

	// QCTP send-side state
	manifest   *manifest.Manifest
	hasher     *manifest.Hasher
	session    *manifest.Session
	hashCancel context.CancelFunc
	fault      *faultSpec

	// Cookie locking the first browser client to the send page.
	cookieMu sync.Mutex
	cookie   http.Cookie

	// Serving configuration, started explicitly via Serve() so that a send
	// session can install its manifest before the first request is accepted.
	listener net.Listener
	secure   bool
	tlsCert  string
	tlsKey   string
}

// ReceiveTo sets the output directory
func (s *Server) ReceiveTo(dir string) error {
	output, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	// Check if the output dir exists
	fileinfo, err := os.Stat(output)
	if err != nil {
		return err
	}
	if !fileinfo.IsDir() {
		return fmt.Errorf("%s is not a valid directory", output)
	}
	s.outputDir = output
	return nil
}

// Send configures the QCTP manifest session served by the server and starts
// the background hasher.
func (s *Server) Send(m *manifest.Manifest, sess *manifest.Session) {
	s.manifest = m
	s.session = sess
	s.hasher = manifest.NewHasher(m)
	ctx, cancel := context.WithCancel(context.Background())
	s.hashCancel = cancel
	s.hasher.Start(ctx)
}

// DisplayQR creates a handler for serving the QR code in the browser
func (s *Server) DisplayQR(url string) {
	const path = "/qr"
	qrImg := qr.RenderImage(url)
	s.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		if err := jpeg.Encode(w, qrImg, nil); err != nil {
			panic(err)
		}
	})
	openBrowser(s.BaseURL + path)
}

// Wait for transfer to be completed, it waits forever if kept alive
func (s *Server) Wait() error {
	<-s.stopChannel
	if s.hashCancel != nil {
		s.hashCancel()
	}
	if err := s.instance.Shutdown(context.Background()); err != nil {
		log.Println(err)
	}
	return nil
}

// Shutdown the server
func (s *Server) Shutdown() {
	s.signalStop()
}

func (s *Server) signalStop() {
	s.shutdownOnce.Do(func() {
		go func() { s.stopChannel <- true }()
	})
}

// New instance of the server
func New(cfg *config.Config) (*Server, error) {

	app := &Server{}
	// Get the address of the configured interface to bind the server to.
	// If `bind` configuration parameter has been configured, it takes precedence
	bind, err := util.GetInterfaceAddress(cfg.Interface)
	if err != nil {
		return &Server{}, err
	}
	if cfg.Bind != "" {
		bind = cfg.Bind
	}
	// Create a listener. If `port: 0`, a random one is chosen
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, cfg.Port))
	if err != nil {
		return nil, err
	}
	// Set the value of computed port
	port := listener.Addr().(*net.TCPAddr).Port
	// Set the host
	host := fmt.Sprintf("%s:%d", bind, port)
	// Get a random path to use
	path := cfg.Path
	if path == "" {
		path = util.GetRandomURLPath()
	}
	// Set the hostname
	hostname := fmt.Sprintf("%s:%d", bind, port)
	// Use external IP when using `interface: any`, unless a FQDN is set
	if bind == "0.0.0.0" && cfg.FQDN == "" {
		fmt.Println("Retrieving the external IP...")
		extIP, err := util.GetExternalIP()
		if err != nil {
			panic(err)
		}
		extIPString := extIP.String()
		fmtstring := "%s:%d"
		if strings.Count(extIPString, ":") >= 2 {
			// IPv6 address, wrap it in [] to add a port
			fmtstring = "[%s]:%d"
		}
		hostname = fmt.Sprintf(fmtstring, extIPString, port)
	}
	// Use a fully-qualified domain name if set
	if cfg.FQDN != "" {
		hostname = fmt.Sprintf("%s:%d", cfg.FQDN, port)
	}
	// Set URLs
	protocol := "http"
	if cfg.Secure {
		protocol = "https"
	}
	app.BaseURL = fmt.Sprintf("%s://%s", protocol, hostname)
	app.SendURL = fmt.Sprintf("%s/send/%s",
		app.BaseURL, path)
	app.ReceiveURL = fmt.Sprintf("%s/receive/%s",
		app.BaseURL, path)
	// Create a server
	httpserver := &http.Server{
		Addr: host,
		TLSConfig: &tls.Config{
			MinVersion:               tls.VersionTLS12,
			CurvePreferences:         []tls.CurveID{tls.CurveP521, tls.CurveP384, tls.CurveP256},
			PreferServerCipherSuites: true,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384: tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
				tls.TLS_RSA_WITH_AES_256_GCM_SHA384:       tls.TLS_RSA_WITH_AES_256_CBC_SHA,
			},
		},
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}
	// Create channel to send message to stop server
	app.stopChannel = make(chan bool)
	// Create cookie used to verify request is coming from first client to connect
	app.cookie = http.Cookie{Name: "qrcp", Value: ""}
	// Gracefully shutdown when an OS signal is received
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		app.signalStop()
	}()
	// Test-only fault injection for chunk responses (QRCP_TEST_FAULT=f:c:n).
	app.fault = parseFaultSpec(os.Getenv("QRCP_TEST_FAULT"))
	// Register all routes on a dedicated mux so the handler is testable.
	app.mux = app.registerRoutes(cfg, path)
	httpserver.Handler = app.mux
	app.instance = httpserver
	app.listener = listener
	app.secure = cfg.Secure
	app.tlsCert = cfg.TlsCert
	app.tlsKey = cfg.TlsKey
	return app, nil
}

// Serve starts accepting connections on the bound listener. For send sessions it
// must be called after Send, so requests can never observe an unconfigured
// server.
func (s *Server) Serve() {
	go func() {
		netListener := tcpKeepAliveListener{s.listener.(*net.TCPListener)}
		if s.secure {
			if err := s.instance.ServeTLS(netListener, s.tlsCert, s.tlsKey); err != http.ErrServerClosed {
				log.Fatalln("error starting the server:", err)
			}
		} else {
			if err := s.instance.Serve(netListener); err != http.ErrServerClosed {
				log.Fatalln("error starting the server", err)
			}
		}
	}()
}

// registerRoutes builds the http.Handler for both the QCTP send session and
// the legacy receive (upload) page.
func (s *Server) registerRoutes(cfg *config.Config, path string) *http.ServeMux {
	mux := http.NewServeMux()
	sendPage := "/send/" + path
	sendAPI := sendPage + "/api/"
	// Transfer page (exact secret URL).
	mux.HandleFunc(sendPage, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != sendPage {
			http.NotFound(w, r)
			return
		}
		if !s.gateCookie(cfg, w, r) {
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s.manifest == nil || s.session == nil {
			http.Error(w, "no transfer configured", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method == http.MethodHead {
			return
		}
		html := strings.ReplaceAll(pages.Transfer, tokenPlaceholder, s.session.Token)
		io.WriteString(w, html)
	})
	// QCTP API.
	mux.HandleFunc(sendAPI, func(w http.ResponseWriter, r *http.Request) {
		if !s.gateCookie(cfg, w, r) {
			return
		}
		s.handleQCTP(cfg, w, r, strings.TrimPrefix(r.URL.Path, sendAPI))
	})
	// Upload handler (serves the upload page) — receive direction, unchanged.
	mux.HandleFunc("/receive/"+path, func(w http.ResponseWriter, r *http.Request) {
		htmlVariables := struct {
			Route string
			File  string
		}{}
		htmlVariables.Route = "/receive/" + path
		switch r.Method {
		case "POST":
			filenames := util.ReadFilenames(s.outputDir)
			reader, err := r.MultipartReader()
			if err != nil {
				fmt.Fprintf(w, "Upload error: %v\n", err)
				log.Printf("Upload error: %v\n", err)
				s.signalStop()
				return
			}
			transferredFiles := []string{}
			progressBar := pb.New64(r.ContentLength)
			progressBar.ShowCounters = false
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				// iIf part.FileName() is empty, skip this iteration.
				if part.FileName() == "" {
					continue
				}
				// Prepare the destination
				fileName := getFileName(filepath.Base(part.FileName()), filenames)
				out, err := os.Create(filepath.Join(s.outputDir, fileName))
				if err != nil {
					// Output to server
					fmt.Fprintf(w, "Unable to create the file for writing: %s\n", err)
					// Output to console
					log.Printf("Unable to create the file for writing: %s\n", err)
					// Send signal to server to shutdown
					s.signalStop()
					return
				}
				defer out.Close()
				// Add name of new file
				filenames = append(filenames, fileName)
				// Write the content from POSTed file to the out
				fmt.Println("Transferring file: ", out.Name())
				progressBar.Prefix(out.Name())
				progressBar.Start()
				buf := make([]byte, 1024)
				for {
					// Read a chunk
					n, err := part.Read(buf)
					if err != nil && err != io.EOF {
						// Output to server
						fmt.Fprintf(w, "Unable to write file to disk: %v", err)
						// Output to console
						fmt.Printf("Unable to write file to disk: %v", err)
						// Send signal to server to shutdown
						s.signalStop()
						return
					}
					if n == 0 {
						break
					}
					// Write a chunk
					if _, err := out.Write(buf[:n]); err != nil {
						// Output to server
						fmt.Fprintf(w, "Unable to write file to disk: %v", err)
						// Output to console
						log.Printf("Unable to write file to disk: %v", err)
						// Send signal to server to shutdown
						s.signalStop()
						return
					}
					progressBar.Add(n)
				}
				transferredFiles = append(transferredFiles, out.Name())
			}
			progressBar.FinishPrint("File transfer completed")
			// Set the value of the variable to the actually transferred files
			htmlVariables.File = strings.Join(transferredFiles, ", ")
			serveTemplate("done", pages.Done, w, htmlVariables)
			if !cfg.KeepAlive {
				s.signalStop()
			}
		case "GET":
			serveTemplate("upload", pages.Upload, w, htmlVariables)
		}
	})
	return mux
}

// gateCookie implements the first-browser-client lock via a session cookie.
// Non-Mozilla clients and keep-alive servers are unrestricted (legacy rule).
func (s *Server) gateCookie(cfg *config.Config, w http.ResponseWriter, r *http.Request) bool {
	if cfg.KeepAlive || !strings.HasPrefix(r.Header.Get("User-Agent"), "Mozilla") {
		return true
	}
	s.cookieMu.Lock()
	defer s.cookieMu.Unlock()
	if s.cookie.Value == "" {
		value, err := util.GetSessionID()
		if err != nil {
			log.Println("Unable to generate session ID", err)
			s.signalStop()
			http.Error(w, "session error", http.StatusInternalServerError)
			return false
		}
		s.cookie.Value = value
		http.SetCookie(w, &s.cookie)
		return true
	}
	rcookie, err := r.Cookie(s.cookie.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	if rcookie.Value != s.cookie.Value {
		http.Error(w, "mismatching cookie", http.StatusBadRequest)
		return false
	}
	return true
}

// handleQCTP dispatches the /api/manifest, /api/chunk and /api/complete
// endpoints of the qrcp chunked transfer protocol.
func (s *Server) handleQCTP(cfg *config.Config, w http.ResponseWriter, r *http.Request, resource string) {
	switch resource {
	case "manifest":
		s.apiManifest(w, r)
	case "chunk":
		s.apiChunk(w, r)
	case "complete":
		s.apiComplete(cfg, w, r)
	default:
		writeAPIError(w, http.StatusNotFound, "unknown endpoint")
	}
}

func (s *Server) apiManifest(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodHead {
		return
	}
	if err := json.NewEncoder(w).Encode(s.hasher.Snapshot(s.session)); err != nil {
		log.Println("manifest encode error:", err)
	}
}

func (s *Server) apiChunk(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	fi, err1 := strconv.Atoi(r.URL.Query().Get("f"))
	ci, err2 := strconv.Atoi(r.URL.Query().Get("c"))
	if err1 != nil || err2 != nil || fi < 0 || fi >= len(s.manifest.Files) {
		writeAPIError(w, http.StatusNotFound, "unknown file or chunk")
		return
	}
	entry := s.manifest.Files[fi]
	if entry.Type != manifest.EntryFile || ci < 0 || int64(ci) >= entry.Chunks {
		writeAPIError(w, http.StatusNotFound, "unknown file or chunk")
		return
	}
	// Open the source before hashing: a missing file is a 404, and a size
	// that no longer matches the manifest snapshot means the sender's file
	// changed mid-transfer (TOCTOU) — fail loudly with 409 instead of
	// serving bytes that could silently mismatch the declared size.
	f, err := os.Open(entry.SourcePath)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "file unavailable: "+err.Error())
		return
	}
	defer f.Close()
	if info, statErr := f.Stat(); statErr != nil || info.Size() != entry.Size {
		writeAPIError(w, http.StatusConflict, "source file changed since the transfer started; please restart the transfer")
		return
	}
	hash, err := s.hasher.ChunkHash(r.Context(), int64(fi), int64(ci))
	if err != nil {
		if errors.Is(err, manifest.ErrSourceChanged) {
			writeAPIError(w, http.StatusConflict, "source file changed since the transfer started; please restart the transfer")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "hashing failure: "+err.Error())
		return
	}
	offset := int64(ci) * s.manifest.ChunkSize
	size := entry.Size - offset
	if size > s.manifest.ChunkSize {
		size = s.manifest.ChunkSize
	}
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		writeAPIError(w, http.StatusInternalServerError, "read failure: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Chunk-Index", strconv.Itoa(ci))
	w.Header().Set("X-Chunk-Sha256", hash)
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	if r.Method == http.MethodHead {
		return
	}
	// Test-only corruption injection (the hash header keeps the true digest
	// so the client detects and re-requests the chunk). Only GET responses
	// consume the fault budget.
	if s.fault != nil && s.fault.consume(fi, ci) && len(buf) > 0 {
		buf[0] ^= 0xff
	}
	if _, err := w.Write(buf); err != nil {
		log.Println("chunk write error:", err)
	}
}

func (s *Server) apiComplete(cfg *config.Config, w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	if !cfg.KeepAlive {
		s.signalStop()
	}
}

// authorize validates the session token and expiry for API requests.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if s.manifest == nil || s.session == nil {
		writeAPIError(w, http.StatusNotFound, "no transfer configured")
		return false
	}
	if s.session.Expired(time.Now()) {
		writeAPIError(w, http.StatusGone, "session expired")
		return false
	}
	token := r.Header.Get("X-QCTP-Token")
	if token == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "qrcp ") {
			token = strings.TrimPrefix(auth, "qrcp ")
		}
	}
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.session.Token)) != 1 {
		writeAPIError(w, http.StatusForbidden, "invalid token")
		return false
	}
	return true
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// openBrowser navigates to a url using the default system browser
func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("failed to open browser on platform: %s", runtime.GOOS)
	}
	if err != nil {
		log.Fatal(err)
	}
}
