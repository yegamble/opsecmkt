package market

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPreviewAdminRendersWithoutEscrowReadinessData(t *testing.T) {
	a, err := New(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/admin", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Escrow status and payment intake") {
		t.Fatalf("admin preview: %d %s", w.Code, w.Body)
	}
}
