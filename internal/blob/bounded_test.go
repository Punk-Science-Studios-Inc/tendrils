package blob

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// serve answers every GET with handler, for a blob whose real bytes are body.
func serve(t *testing.T, handler func(w http.ResponseWriter, body []byte)) (*Client, string, []byte) {
	t.Helper()
	body := bytes.Repeat([]byte("sealed-bytes-"), 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(w, body) }))
	t.Cleanup(srv.Close)
	c := New(srv.URL, testIdentity(t))
	c.StallTimeout = 200 * time.Millisecond
	return c, hexHash(body), body
}

func TestDownloadStallIsAbandoned(t *testing.T) {
	release := make(chan struct{})
	c, sha, _ := serve(t, func(w http.ResponseWriter, body []byte) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body[:100])
		w.(http.Flusher).Flush()
		<-release
	})
	t.Cleanup(func() { close(release) })
	start := time.Now()
	_, err := c.DownloadSize(context.Background(), sha, 13000)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("err = %v, want ErrStalled", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %s to notice a stall", time.Since(start))
	}
}

// A slow transfer that keeps moving is never cut off, however long it takes.
func TestSlowButMovingDownloadCompletes(t *testing.T) {
	c, sha, body := serve(t, func(w http.ResponseWriter, body []byte) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		for i := 0; i < len(body); i += 1300 {
			w.Write(body[i:min(i+1300, len(body))])
			w.(http.Flusher).Flush()
			time.Sleep(80 * time.Millisecond)
		}
	})
	got, err := c.DownloadSize(context.Background(), sha, int64(len(body)))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadRejectsWrongSizes(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, body []byte){
		"declared too large": func(w http.ResponseWriter, body []byte) {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)+1))
			w.Write(append(body, 'x'))
		},
		"undeclared and too long": func(w http.ResponseWriter, body []byte) {
			w.(http.Flusher).Flush()
			w.Write(append(body, bytes.Repeat([]byte("x"), 5000)...))
		},
		"undeclared and short": func(w http.ResponseWriter, body []byte) {
			w.(http.Flusher).Flush()
			w.Write(body[:len(body)-1])
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			c, sha, body := serve(t, h)
			if _, err := c.DownloadSize(context.Background(), sha, int64(len(body))); !errors.Is(err, ErrWrongSize) {
				t.Fatalf("err = %v, want ErrWrongSize", err)
			}
		})
	}
}

func TestDownloadTruncatedBodyFails(t *testing.T) {
	c, sha, body := serve(t, func(w http.ResponseWriter, body []byte) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body[:len(body)/2])
	})
	if _, err := c.DownloadSize(context.Background(), sha, int64(len(body))); err == nil {
		t.Fatal("a truncated body was accepted")
	}
}

func TestDownloadCorruptBodyFails(t *testing.T) {
	c, sha, body := serve(t, func(w http.ResponseWriter, body []byte) {
		bad := append([]byte(nil), body...)
		bad[0] ^= 1
		w.Write(bad)
	})
	if _, err := c.DownloadSize(context.Background(), sha, int64(len(body))); err == nil {
		t.Fatal("corrupt bytes were accepted")
	}
}

// Without a known size, a body larger than MaxBytes is refused rather than read.
func TestUnknownSizeIsBoundedByMaxBytes(t *testing.T) {
	c, sha, _ := serve(t, func(w http.ResponseWriter, body []byte) {
		w.(http.Flusher).Flush()
		w.Write(body)
	})
	c.MaxBytes = 4096
	if _, err := c.Download(context.Background(), sha); !errors.Is(err, ErrWrongSize) {
		t.Fatalf("err = %v, want ErrWrongSize", err)
	}
}

// A server that stalls is skipped for the next one rather than holding the pass.
func TestPoolFallsThroughStalledServer(t *testing.T) {
	release := make(chan struct{})
	stuck, sha, body := serve(t, func(w http.ResponseWriter, body []byte) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.(http.Flusher).Flush()
		<-release
	})
	t.Cleanup(func() { close(release) })
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	t.Cleanup(good.Close)

	p := NewPoolWithClients(stuck, New(good.URL, testIdentity(t)))
	got, err := p.DownloadSize(context.Background(), sha, int64(len(body)))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("err = %v", err)
	}
}
