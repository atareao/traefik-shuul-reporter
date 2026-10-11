package main

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// countingFlusher is a ResponseWriter whose Flush calls are counted, so tests
// can assert that captureWriter.Flush() delegates to the wrapped writer.
type countingFlusher struct {
	*httptest.ResponseRecorder
	flushes int
}

func (w *countingFlusher) Flush() { w.flushes++ }

// hijackRecorder records whether Hijack was delegated to it.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (w *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, nil
}

// TestCaptureWriterFlusher is the test the Yaegi plugin could never pass: the
// compiled captureWriter must satisfy http.Flusher and its Flush() must
// delegate to the underlying writer.
func TestCaptureWriterFlusher(t *testing.T) {
	under := &countingFlusher{ResponseRecorder: httptest.NewRecorder()}
	cw := &captureWriter{ResponseWriter: under, minStatus: 300}

	if _, ok := http.ResponseWriter(cw).(http.Flusher); !ok {
		t.Fatal("captureWriter must implement http.Flusher (compiled Go)")
	}

	cw.Flush()
	if under.flushes != 1 {
		t.Fatalf("Flush() did not delegate to the underlying writer: flushes=%d, want 1", under.flushes)
	}
}

func TestCaptureStatus(t *testing.T) {
	t.Run("first WriteHeader wins and reports", func(t *testing.T) {
		rec := httptest.NewRecorder()
		var got []reportPayload
		cw := &captureWriter{
			ResponseWriter: rec,
			minStatus:      300,
			req:            httptest.NewRequest(http.MethodGet, "http://example.com/up", nil),
			enqueue:        func(p reportPayload) { got = append(got, p) },
		}

		cw.WriteHeader(http.StatusInternalServerError)
		cw.WriteHeader(http.StatusOK) // second call must not re-capture

		if cw.statusCode != http.StatusInternalServerError {
			t.Fatalf("statusCode = %d, want 500", cw.statusCode)
		}
		if len(got) != 1 {
			t.Fatalf("enqueued %d reports, want exactly 1", len(got))
		}
		if got[0].StatusCode != 500 || got[0].Path != "/up" {
			t.Fatalf("unexpected report: %+v", got[0])
		}
	})

	t.Run("Write without header implies 200 and does not report", func(t *testing.T) {
		rec := httptest.NewRecorder()
		enqueued := false
		cw := &captureWriter{
			ResponseWriter: rec,
			minStatus:      300,
			req:            httptest.NewRequest(http.MethodGet, "http://example.com/ok", nil),
			enqueue:        func(reportPayload) { enqueued = true },
		}

		if _, err := cw.Write([]byte("hi")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if cw.statusCode != http.StatusOK {
			t.Fatalf("implicit status = %d, want 200", cw.statusCode)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("recorder code = %d, want 200", rec.Code)
		}
		if enqueued {
			t.Fatal("200 must not be enqueued")
		}
	})

	t.Run("status below minStatus is not enqueued", func(t *testing.T) {
		rec := httptest.NewRecorder()
		enqueued := false
		cw := &captureWriter{
			ResponseWriter: rec,
			minStatus:      500,
			req:            httptest.NewRequest(http.MethodGet, "http://example.com/x", nil),
			enqueue:        func(reportPayload) { enqueued = true },
		}

		cw.WriteHeader(http.StatusNotFound) // 404 < 500
		if enqueued {
			t.Fatal("status below minStatus must not be enqueued")
		}
		if cw.statusCode != http.StatusNotFound {
			t.Fatalf("statusCode = %d, want 404 (status is still captured)", cw.statusCode)
		}
	})

	t.Run("Hijack delegates to the underlying writer", func(t *testing.T) {
		under := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
		cw := &captureWriter{ResponseWriter: under, minStatus: 300}

		if _, _, err := cw.Hijack(); err != nil {
			t.Fatalf("Hijack: %v", err)
		}
		if !under.hijacked {
			t.Fatal("Hijack did not delegate to the underlying writer")
		}
	})
}
