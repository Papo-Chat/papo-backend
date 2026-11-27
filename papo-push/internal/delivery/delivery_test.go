package delivery

import (
	"reflect"
	"testing"
)

// TestMergeResults verifica que a união dos resultados preserva tokens
// entregues, inválidos e os flags de retryable/permanent.
func TestMergeResults(t *testing.T) {
	a := NewTestResult(TargetResult{Token: "a1", Status: StatusSent}, TargetResult{Token: "a2", Status: StatusInvalidToken})
	b := NewTestResult(TargetResult{Token: "b1", Status: StatusRetryableError}, TargetResult{Token: "b2", Status: StatusSent})
	c := NewTestResult(TargetResult{Token: "c1", Status: StatusPermanentError})

	merged := MergeResults(a, b, c)
	if got := merged.SentTokens(); !reflect.DeepEqual(got, []string{"a1", "b2"}) {
		t.Errorf("SentTokens = %v", got)
	}
	if got := merged.InvalidTokens(); !reflect.DeepEqual(got, []string{"a2"}) {
		t.Errorf("InvalidTokens = %v", got)
	}
	if !merged.HasRetryable() || !merged.HasPermanent() {
		t.Errorf("flags: retryable=%v permanent=%v", merged.HasRetryable(), merged.HasPermanent())
	}
	if len(merged.TargetResults()) != 5 {
		t.Errorf("TargetResults = %d, esperado 5", len(merged.TargetResults()))
	}
}

// TestMergeResultsEmpty verifica que sem partes (ou só partes vazias) o
// resultado não tem falhas.
func TestMergeResultsEmpty(t *testing.T) {
	for _, merged := range []Result{MergeResults(), MergeResults(Result{}, Result{})} {
		if merged.HasRetryable() || merged.HasPermanent() || len(merged.SentTokens()) != 0 || len(merged.InvalidTokens()) != 0 {
			t.Errorf("resultado vazio inesperado: %+v", merged)
		}
	}
}
