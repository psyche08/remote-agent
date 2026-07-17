package logupload

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadOnceUploadsIncrementally(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "remote-coding.log")
	state := filepath.Join(dir, "state.json")
	if err := os.WriteFile(src, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got [][]byte
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/_pt/logs/remocoding/device-a" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		got = append(got, b)
		return okResponse(), nil
	})}
	opts := Options{
		RelayURL:   "https://relay.example.test",
		Namespace:  "remocoding",
		DeviceID:   "device-a",
		StatePath:  state,
		Sources:    []string{src},
		HTTPClient: client,
	}
	res, err := UploadOnce(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.UploadedBytes != 6 || len(got) != 1 || string(got[0]) != "hello\n" {
		t.Fatalf("first upload res=%#v got=%q", res, got)
	}
	if err := os.WriteFile(src, []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = UploadOnce(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.UploadedBytes != 6 || len(got) != 2 || string(got[1]) != "world\n" {
		t.Fatalf("second upload res=%#v got=%q", res, got)
	}
}

func TestUploadOnceResetsOffsetAfterTruncate(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "remote-coding.log")
	state := filepath.Join(dir, "state.json")
	if err := os.WriteFile(state, []byte(`{"offsets":{"`+src+`":99}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		return okResponse(), nil
	})}
	_, err := UploadOnce(context.Background(), Options{
		RelayURL:   "https://relay.example.test",
		Namespace:  "remocoding",
		DeviceID:   "device-a",
		StatePath:  state,
		Sources:    []string{src},
		HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "new\n" {
		t.Fatalf("got %q", got)
	}
}





type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func okResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}
