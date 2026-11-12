package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNativeStreamRebindSwapsDeadUpstreamInPlace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/live1/"):
			w.WriteHeader(http.StatusForbidden)
		case strings.HasPrefix(r.URL.Path, "/live2/"):
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, "#EXTM3U\n#EXT-X-ENDLIST\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := newNativeStreamServer(engine.downloader)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.server.Close()
	media := providerMedia{URL: upstream.URL + "/live1/master.m3u8", Referer: "https://example.test/watch"}
	address, token := stream.nativeOpen(media)
	defer stream.nativeRelease(token)
	refreshes := 0
	stream.nativeBindRefresh(token, func(context.Context) (providerMedia, error) {
		refreshes++
		return providerMedia{URL: upstream.URL + "/live2/master.m3u8", Referer: media.Referer}, nil
	})
	first, err := http.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusForbidden {
		t.Fatalf("dead upstream error was masked: HTTP %d", first.StatusCode)
	}
	if refreshes != 1 {
		t.Fatalf("refresh was not invoked once: %d", refreshes)
	}
	second, err := http.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("rebinded route not playable: HTTP %d", second.StatusCode)
	}
	body, _ := io.ReadAll(second.Body)
	if !strings.HasPrefix(string(body), "#EXTM3U") {
		t.Fatalf("rewritten playlist invalid: %s", body)
	}
	if refreshes != 1 {
		t.Fatalf("healthy upstream refreshed again: %d", refreshes)
	}
}

func TestNativeStreamRebindRewritesSegmentsUnderNewRoot(t *testing.T) {
	var requests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		switch r.URL.Path {
		case "/live1/master.m3u8":
			w.WriteHeader(http.StatusForbidden)
		case "/live2/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, "#EXTM3U\n#EXTINF:4,\nsegment.ts\n#EXT-X-ENDLIST\n")
		case "/live2/segment.ts":
			io.WriteString(w, "segment")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := newNativeStreamServer(engine.downloader)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.server.Close()
	media := providerMedia{URL: upstream.URL + "/live1/master.m3u8", Referer: "https://example.test/watch"}
	address, token := stream.nativeOpen(media)
	defer stream.nativeRelease(token)
	stream.nativeBindRefresh(token, func(context.Context) (providerMedia, error) {
		return providerMedia{URL: upstream.URL + "/live2/master.m3u8", Referer: media.Referer}, nil
	})
	first, err := http.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	second, err := http.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if !strings.Contains(string(body), "http") {
		t.Fatalf("playlist was not rewritten: %s", body)
	}
	segment := ""
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "http") {
			segment = line
		}
	}
	response, err := http.Get(segment)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("segment under rebased root is unreachable: HTTP %d", response.StatusCode)
	}
}

func TestNativeStreamRebindRejectsKeyRotation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := newNativeStreamServer(engine.downloader)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.server.Close()
	media := providerMedia{URL: upstream.URL + "/master.m3u8", Referer: "https://example.test/watch", CENCKey: []byte("0123456789abcdef")}
	_, token := stream.nativeOpen(media)
	defer stream.nativeRelease(token)
	stream.nativeBindRefresh(token, func(context.Context) (providerMedia, error) {
		return providerMedia{URL: upstream.URL + "/other.m3u8", Referer: media.Referer, CENCKey: []byte("fedcba9876543210")}, nil
	})
	session := stream.sessions[token]
	if session.nativeRebind(context.Background()) {
		t.Fatal("key-rotated refresh must not swap the route in place")
	}
	session.mu.Lock()
	asset := session.assets[session.rootKey]
	session.mu.Unlock()
	if asset.address != media.URL {
		t.Fatalf("route was swapped despite key rotation: %s", asset.address)
	}
}

func TestNativeStreamRebindCapsRefreshAttempts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := newNativeStreamServer(engine.downloader)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.server.Close()
	media := providerMedia{URL: upstream.URL + "/master.m3u8", Referer: "https://example.test/watch"}
	_, token := stream.nativeOpen(media)
	defer stream.nativeRelease(token)
	calls := 0
	stream.nativeBindRefresh(token, func(context.Context) (providerMedia, error) {
		calls++
		return providerMedia{URL: upstream.URL + fmt.Sprintf("/live%d/master.m3u8", calls), Referer: media.Referer}, nil
	})
	session := stream.sessions[token]
	for attempt := 0; attempt < streamMaxRebinds+2; attempt++ {
		session.mu.Lock()
		session.lastRebind = session.lastRebind.Add(-30 * time.Second)
		session.mu.Unlock()
		_ = session.nativeRebind(context.Background())
	}
	if calls != streamMaxRebinds {
		t.Fatalf("refresh attempts were not capped: got %d want %d", calls, streamMaxRebinds)
	}
}

func TestNativeStreamRebindNoOpWithoutResolver(t *testing.T) {
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := newNativeStreamServer(engine.downloader)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.server.Close()
	media := providerMedia{URL: "https://media.example.test/master.m3u8", Playlist: "#EXTM3U\n#EXT-X-ENDLIST\n"}
	_, token := stream.nativeOpen(media)
	defer stream.nativeRelease(token)
	session := stream.sessions[token]
	if session.nativeRebind(context.Background()) {
		t.Fatal("rebind without a resolver must stay a no-op")
	}
}
