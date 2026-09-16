package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claudiodangelis/qrcp/config"
	"github.com/claudiodangelis/qrcp/manifest"
)

func writeFixture(t *testing.T, root string, sizes map[string]int64) {
	t.Helper()
	for rel, size := range sizes {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		// Pseudo-random but deterministic content.
		rng := rand.New(rand.NewSource(int64(size) + int64(len(rel))))
		if _, err := io.CopyN(f, rng, size); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	if err := os.MkdirAll(filepath.Join(root, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func newQCTPTestServer(t *testing.T, chunkSize int64, ttl time.Duration, sizes map[string]int64, args []string) (*httptest.Server, *Server, *manifest.Manifest, *manifest.Session) {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, root, sizes)
	if args == nil {
		args = []string{root}
	}
	m, err := manifest.Build(args, chunkSize)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := manifest.NewSession(ttl)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		stopChannel: make(chan bool, 1),
		cookie:      http.Cookie{Name: "qrcp"},
	}
	s.Send(m, sess)
	h := s.registerRoutes(&config.Config{KeepAlive: false}, "secret")
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	t.Cleanup(s.hashCancel)
	return ts, s, m, sess
}

func req(t *testing.T, ts *httptest.Server, method, url, token string, body io.Reader) *http.Response {
	t.Helper()
	r, err := http.NewRequest(method, ts.URL+url, body)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("User-Agent", "qrcp-test/1.0")
	if token != "" {
		r.Header.Set("X-QCTP-Token", token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func waitHashing(t *testing.T, h *manifest.Hasher) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if h.HashingDone() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hashing did not finish")
}

func TestManifestEndpoint(t *testing.T) {
	sizes := map[string]int64{"a.dat": 0, "sub/b.dat": 65}
	ts, _, m, sess := newQCTPTestServer(t, 64, time.Minute, sizes, nil)
	resp := req(t, ts, "GET", "/send/secret/api/manifest", sess.Token, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("manifest status = %d", resp.StatusCode)
	}
	var dto manifest.DTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.Protocol != manifest.ProtocolVersion || dto.Token != sess.Token ||
		dto.ChunkSize != 64 || len(dto.Files) != len(m.Files) {
		t.Fatalf("unexpected dto: %+v", dto)
	}
	if dto.CreatedAt.IsZero() || dto.ExpiresAt.IsZero() {
		t.Fatal("session timestamps missing")
	}
}

func TestChunkEndpointBytesAndHeaders(t *testing.T) {
	sizes := map[string]int64{"empty": 0, "one.dat": 1, "exact.dat": 64, "tail.dat": 65, "big.dat": 200}
	ts, _, m, sess := newQCTPTestServer(t, 64, time.Minute, sizes, nil)

	// Expected bytes + hashes straight from disk.
	for fi, e := range m.Files {
		if e.Type != manifest.EntryFile {
			continue
		}
		raw, err := os.ReadFile(e.SourcePath)
		if err != nil {
			t.Fatal(err)
		}
		for ci := int64(0); ci < e.Chunks; ci++ {
			resp := req(t, ts, "GET",
				fmt.Sprintf("/send/secret/api/chunk?f=%d&c=%d", fi, ci), sess.Token, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("chunk %d:%d status = %d", fi, ci, resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			start := ci * 64
			want := raw[start:min64(start+64, int64(len(raw)))]
			if string(body) != string(want) {
				t.Fatalf("chunk %d:%d bytes mismatch (%d vs %d)", fi, ci, len(body), len(want))
			}
			sum := sha256.Sum256(body)
			if resp.Header.Get("X-Chunk-Sha256") != hex.EncodeToString(sum[:]) {
				t.Fatalf("chunk %d:%d hash header mismatch", fi, ci)
			}
			if resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("cache-control = %q", resp.Header.Get("Cache-Control"))
			}
			if cl := resp.Header.Get("Content-Length"); cl != fmt.Sprintf("%d", len(body)) {
				t.Fatalf("content-length = %q, want %d", cl, len(body))
			}
		}
	}

	// HEAD must return headers but no body.
	resp := req(t, ts, "HEAD", "/send/secret/api/chunk?f=1&c=0", sess.Token, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("HEAD status = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Chunk-Sha256") == "" {
		t.Fatal("HEAD missing hash header")
	}
}

func TestAPIErrorCodes(t *testing.T) {
	sizes := map[string]int64{"a.dat": 65}
	ts, _, m, sess := newQCTPTestServer(t, 64, time.Minute, sizes, nil)

	// Missing token.
	if r := req(t, ts, "GET", "/send/secret/api/manifest", "", nil); r.StatusCode != 403 {
		t.Fatalf("no token: status = %d, want 403", r.StatusCode)
	}
	// Wrong token.
	if r := req(t, ts, "GET", "/send/secret/api/manifest", "deadbeef", nil); r.StatusCode != 403 {
		t.Fatalf("bad token: status = %d, want 403", r.StatusCode)
	}
	// Bad file / chunk indices.
	if r := req(t, ts, "GET", "/send/secret/api/chunk?f=99&c=0", sess.Token, nil); r.StatusCode != 404 {
		t.Fatalf("bad file: status = %d, want 404", r.StatusCode)
	}
	fileIdx := -1
	for fi, e := range m.Files {
		if e.Type == manifest.EntryFile {
			fileIdx = fi
		}
	}
	if r := req(t, ts, "GET", fmt.Sprintf("/send/secret/api/chunk?f=%d&c=99", fileIdx), sess.Token, nil); r.StatusCode != 404 {
		t.Fatalf("bad chunk: status = %d, want 404", r.StatusCode)
	}
	// Unknown endpoint.
	if r := req(t, ts, "GET", "/send/secret/api/nope", sess.Token, nil); r.StatusCode != 404 {
		t.Fatalf("unknown endpoint: status = %d", r.StatusCode)
	}
	// Method not allowed.
	if r := req(t, ts, "DELETE", "/send/secret/api/manifest", sess.Token, nil); r.StatusCode != 405 {
		t.Fatalf("method: status = %d, want 405", r.StatusCode)
	}
	// Expired session -> 410.
	m2, err := manifest.Build([]string{m.Files[fileIdx].SourcePath}, 64)
	if err != nil {
		t.Fatal(err)
	}
	expired := &manifest.Session{Token: sess.Token + "ee", CreatedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)}
	s2 := &Server{stopChannel: make(chan bool, 1), cookie: http.Cookie{Name: "qrcp"}}
	s2.Send(m2, expired)
	ts2 := httptest.NewServer(s2.registerRoutes(&config.Config{}, "x"))
	defer ts2.Close()
	defer s2.hashCancel()
	r := req(t, ts2, "GET", "/send/x/api/manifest", expired.Token, nil)
	if r.StatusCode != 410 {
		t.Fatalf("expired: status = %d, want 410", r.StatusCode)
	}
	var apiErr map[string]string
	json.NewDecoder(r.Body).Decode(&apiErr)
	if apiErr["error"] != "session expired" {
		t.Fatalf("error body = %v", apiErr)
	}
}

func TestChunkEndpointSourceChangedOrMissing(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, map[string]int64{"change.dat": 64})
	src := filepath.Join(root, "change.dat")
	ts, _, _, sess := newQCTPTestServer(t, 64, time.Minute, nil, []string{src})
	url := "/send/secret/api/chunk?f=0&c=0"
	if r := req(t, ts, "GET", url, sess.Token, nil); r.StatusCode != 200 {
		t.Fatalf("baseline chunk status = %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	// Growing the source after the manifest snapshot must fail loudly (409), not
	// serve bytes that silently mismatch the declared size.
	f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if r := req(t, ts, "GET", url, sess.Token, nil); r.StatusCode != http.StatusConflict {
		t.Fatalf("grown source: status = %d, want 409", r.StatusCode)
	} else {
		r.Body.Close()
	}
	// A removed source is a 404.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if r := req(t, ts, "GET", url, sess.Token, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("missing source: status = %d, want 404", r.StatusCode)
	} else {
		r.Body.Close()
	}
}

func TestCompleteShutdownSignal(t *testing.T) {
	sizes := map[string]int64{"a.dat": 10}
	ts, s, _, sess := newQCTPTestServer(t, 64, time.Minute, sizes, nil)

	// Non-keep-alive: complete must signal stop.
	r := req(t, ts, "POST", "/send/secret/api/complete", sess.Token, nil)
	if r.StatusCode != 200 {
		t.Fatalf("complete status = %d", r.StatusCode)
	}
	select {
	case <-time.After(2 * time.Second):
		t.Fatal("server was not signaled to stop after /api/complete")
	case <-s.stopChannel:
	}
}

func TestCompleteKeepAliveDoesNotStop(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, map[string]int64{"a.dat": 10})
	m, _ := manifest.Build([]string{root}, 64)
	sess, _ := manifest.NewSession(time.Minute)
	s := &Server{stopChannel: make(chan bool, 1), cookie: http.Cookie{Name: "qrcp"}}
	s.Send(m, sess)
	ts := httptest.NewServer(s.registerRoutes(&config.Config{KeepAlive: true}, "k"))
	defer ts.Close()
	defer s.hashCancel()

	r := req(t, ts, "POST", "/send/k/api/complete", sess.Token, nil)
	if r.StatusCode != 200 {
		t.Fatalf("complete status = %d", r.StatusCode)
	}
	select {
	case v := <-s.stopChannel:
		t.Fatalf("keep-alive server unexpectedly signaled stop: %v", v)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestFaultInjection(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, map[string]int64{"a.dat": 200})
	m, _ := manifest.Build([]string{root}, 64)
	sess, _ := manifest.NewSession(time.Minute)
	s := &Server{
		stopChannel: make(chan bool, 1),
		cookie:      http.Cookie{Name: "qrcp"},
		fault:       &faultSpec{fileIndex: 1, chunkIndex: 2, remaining: 1},
	}
	s.Send(m, sess)
	ts := httptest.NewServer(s.registerRoutes(&config.Config{}, "f"))
	defer ts.Close()
	defer s.hashCancel()

	// File index 1 = a.dat (index 0 is the root directory), chunk 2.
	url := "/send/f/api/chunk?f=1&c=2"
	r1 := req(t, ts, "GET", url, sess.Token, nil)
	b1, _ := io.ReadAll(r1.Body)
	r1.Body.Close()
	sum1 := sha256.Sum256(b1)
	if hex.EncodeToString(sum1[:]) == r1.Header.Get("X-Chunk-Sha256") {
		t.Fatal("fault did not corrupt the first response")
	}
	r2 := req(t, ts, "GET", url, sess.Token, nil)
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	sum2 := sha256.Sum256(b2)
	if hex.EncodeToString(sum2[:]) != r2.Header.Get("X-Chunk-Sha256") {
		t.Fatal("second response still corrupted")
	}
}

func TestConcurrentChunkAssembly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 120MiB concurrency test in -short mode")
	}
	const total = 120 * 1024 * 1024
	root := t.TempDir()
	writeFixture(t, root, map[string]int64{"big.bin": total, "small.dat": 5000})
	chunkSize := int64(1024 * 1024)
	m, _ := manifest.Build([]string{root}, chunkSize)
	sess, _ := manifest.NewSession(time.Minute)
	s := &Server{stopChannel: make(chan bool, 1), cookie: http.Cookie{Name: "qrcp"}}
	s.Send(m, sess)
	ts := httptest.NewServer(s.registerRoutes(&config.Config{}, "c"))
	defer ts.Close()
	defer s.hashCancel()

	for _, e := range m.Files {
		if e.Type != manifest.EntryFile {
			continue
		}
		indices := make([]int64, e.Chunks)
		for i := range indices {
			indices[i] = int64(i)
		}
		rand.Shuffle(len(indices), func(i, j int) { indices[i], indices[j] = indices[j], indices[i] })
		assembled := make([][]byte, e.Chunks)
		jobs := make(chan int64)
		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ci := range jobs {
					resp := req(t, ts, "GET",
						fmt.Sprintf("/send/c/api/chunk?f=%d&c=%d", e.Index, ci), sess.Token, nil)
					if resp.StatusCode != 200 {
						t.Errorf("chunk %d status %d", ci, resp.StatusCode)
						resp.Body.Close()
						return
					}
					buf, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					assembled[ci] = buf
				}
			}()
		}
		for _, ci := range indices {
			jobs <- ci
		}
		close(jobs)
		wg.Wait()

		raw, err := os.ReadFile(e.SourcePath)
		if err != nil {
			t.Fatal(err)
		}
		joined := make([]byte, 0, e.Size)
		for _, b := range assembled {
			joined = append(joined, b...)
		}
		if int64(len(joined)) != e.Size {
			t.Fatalf("%s assembled size = %d, want %d", e.Path, len(joined), e.Size)
		}
		if string(joined) != string(raw) {
			t.Fatalf("%s assembled bytes differ from source", e.Path)
		}
	}
	// Hasher must finalize even with parallel on-demand pressure.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	deadline := ctx.Done()
	for !s.hasher.HashingDone() {
		select {
		case <-deadline:
			t.Fatal("hasher never finalized under concurrency")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestTransferPageInjectsToken(t *testing.T) {
	sizes := map[string]int64{"a.dat": 10}
	ts, _, _, sess := newQCTPTestServer(t, 64, time.Minute, sizes, nil)
	r := req(t, ts, "GET", "/send/secret", sess.Token, nil)
	if r.StatusCode != 200 {
		t.Fatalf("page status = %d", r.StatusCode)
	}
	body, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(body), sess.Token) {
		t.Fatal("token not injected into transfer page")
	}
	if strings.Contains(string(body), "<script src=\"http") {
		t.Fatal("transfer page loads external scripts")
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
