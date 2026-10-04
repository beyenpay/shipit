package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testArchive = "app-v1-linux-amd64.tar.gz"

func sum(b string) string {
	h := sha256.Sum256([]byte(b))
	return hex.EncodeToString(h[:])
}

type fakeGH struct {
	*httptest.Server
	assetAccept, assetAuth string
}

func newFakeGH(t *testing.T, payload, checksums string) *fakeGH {
	t.Helper()
	g := &fakeGH{}
	mux := http.NewServeMux()
	json := func(w http.ResponseWriter, s string) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, s)
	}
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json(w, `{"tag_name":"v2","assets":[]}`)
	})
	mux.HandleFunc("/repos/o/r/releases/tags/v1", func(w http.ResponseWriter, r *http.Request) {
		json(w, fmt.Sprintf(`{"tag_name":"v1","assets":[
                      {"id":1,"name":%q,"size":%d,"state":"uploaded"},
                      {"id":2,"name":"checksums.txt","size":%d,"state":"uploaded"}]}`,
			testArchive, len(payload), len(checksums)))
	})
	mux.HandleFunc("/repos/o/r/releases/tags/draft", func(w http.ResponseWriter, r *http.Request) {
		json(w, `{"tag_name":"draft","draft":true}`)
	})
	mux.HandleFunc("/repos/o/r/releases/tags/other", func(w http.ResponseWriter, r *http.Request) {
		json(w, `{"tag_name":"v9"}`)
	})
	mux.HandleFunc("/repos/o/r/releases/tags/missing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json(w, `{"message":"Not Found"}`)
	})
	mux.HandleFunc("/repos/o/r/releases/assets/1", func(w http.ResponseWriter, r *http.Request) {
		g.assetAccept = r.Header.Get("Accept")
		g.assetAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, "/blob/1", http.StatusFound)
	})
	mux.HandleFunc("/blob/1", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, payload) })
	mux.HandleFunc("/repos/o/r/releases/assets/2", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, checksums)
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

func (g *fakeGH) client() *Client {
	return &Client{BaseURL: g.URL, Token: "tok", HTTP: g.Client()}
}

func TestRelease(t *testing.T) {
	g := newFakeGH(t, "", "")
	c := g.client()
	ctx := context.Background()

	if r, err := c.Release(ctx, "o/r", ""); err != nil || r.Tag != "v2" {
		t.Errorf("latest: %v, %v", r, err)
	}
	if r, err := c.Release(ctx, "o/r", "v1"); err != nil || len(r.Assets) != 2 {
		t.Errorf("tag: %v, %v", r, err)
	}
	for _, tc := range []struct{ tag, want string }{
		{"draft", "draft"},
		{"other", "returned release"},
		{"missing", "Not Found"},
	} {
		if _, err := c.Release(ctx, "o/r", tc.tag); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("tag %s: err = %v, want %q", tc.tag, err, tc.want)
		}
	}
	if _, err := c.Release(ctx, "o/r", "missing"); !strings.Contains(err.Error(), "token") {
		t.Errorf("404 lacks hint: %v", err)
	}
	for _, repo := range []string{"nope", "a/b/c", "/x", ".a/b", "a/.."} {
		if _, err := c.Release(ctx, repo, "v1"); err == nil || !strings.Contains(err.Error(), "invalid repo") {
			t.Errorf("repo %q: err = %v", repo, err)
		}
	}
}

func TestDownloadVerified(t *testing.T) {
	payload := "tarball-bytes"
	g := newFakeGH(t, payload, sum(payload)+"  "+testArchive+"\n"+strings.Repeat("0", 64)+" *other\n")
	c := g.client()
	ctx := context.Background()

	rel, err := c.Release(ctx, "o/r", "v1")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "dl")
	if err := c.DownloadVerified(ctx, "o/r", rel, testArchive, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != payload {
		t.Errorf("payload = %q", b)
	}
	if g.assetAccept != "application/octet-stream" || g.assetAuth != "Bearer tok" {
		t.Errorf("asset request headers: %q %q", g.assetAccept, g.assetAuth)
	}

	err = c.DownloadVerified(ctx, "o/r", rel, "app-v1-linux-arm64.tar.gz", filepath.Join(t.TempDir(), "x"))
	if err == nil || !strings.Contains(err.Error(), "has no asset") || !strings.Contains(err.Error(), testArchive) {
		t.Errorf("missing asset error = %v", err)
	}
}

func TestDownloadVerifiedFailures(t *testing.T) {
	payload := "tarball-bytes"
	tests := []struct{ name, checksums, want string }{
		{"mismatch", strings.Repeat("a", 64) + "  " + testArchive + "\n", "sha256 mismatch"},
		{"no entry", sum(payload) + "  something-else\n", "no entry"},
		{"malformed", "garbage\n", "malformed"},
		{"bad hash", "zz  " + testArchive + "\n", "invalid sha256"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newFakeGH(t, payload, tt.checksums)
			c := g.client()
			rel, err := c.Release(context.Background(), "o/r", "v1")
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(t.TempDir(), "dl")
			err = c.DownloadVerified(context.Background(), "o/r", rel, testArchive, dst)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if _, err := os.Stat(dst); err == nil {
				t.Error("download left behind after failure")
			}
		})
	}
}

func TestParseChecksums(t *testing.T) {
	h := strings.Repeat("ab", 32)
	m, err := ParseChecksums([]byte("# c\n\n" + h + "  a.tar.gz\n" + strings.ToUpper(h) + " *./b.tar.gz\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m["a.tar.gz"] != h || m["b.tar.gz"] != h || len(m) != 2 {
		t.Errorf("m = %v", m)
	}
}
