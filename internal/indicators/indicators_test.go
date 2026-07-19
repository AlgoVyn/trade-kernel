package indicators

import (
	"math"
	"testing"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSMA(t *testing.T) {
	s := NewSMA(3)
	if !math.IsNaN(s.Update(1)) {
		t.Fatal("SMA should be NaN before window fills")
	}
	s.Update(2)
	if got := s.Update(3); !almost(got, 2) {
		t.Fatalf("SMA(1,2,3) = %v, want 2", got)
	}
	if got := s.Update(6); !almost(got, 11.0/3.0) {
		t.Fatalf("SMA(2,3,6) = %v, want %v", got, 11.0/3.0)
	}
	// Peek must not mutate.
	want := s.Value()
	p := s.Peek(9)
	if !almost(p, 6) {
		t.Fatalf("Peek(9) = %v, want 6", p)
	}
	if !almost(s.Value(), want) {
		t.Fatalf("Peek mutated state: %v != %v", s.Value(), want)
	}
}

func TestEMA(t *testing.T) {
	// Reference: EMA(n=3), k=0.5 over [1..5].
	e := NewEMA(3)
	vals := []float64{1, 2, 3, 4, 5}
	want := []float64{1, 1.5, 2.25, 3.125, 4.0625}
	for i, v := range vals {
		if got := e.Update(v); !almost(got, want[i]) {
			t.Fatalf("EMA step %d = %v, want %v", i, got, want[i])
		}
	}
	p := e.Peek(6)
	if !almost(p, 5.03125) {
		t.Fatalf("Peek = %v, want 5.03125", p)
	}
	if !almost(e.Value(), 4.0625) {
		t.Fatalf("Peek mutated state")
	}
}

func TestEMAUndo(t *testing.T) {
	e := NewEMA(3)
	e.Update(1)
	e.Update(2)
	e.Update(3) // value 2.25
	e.Undo(3)
	if !almost(e.Value(), 1.5) {
		t.Fatalf("after Undo(3) = %v, want 1.5", e.Value())
	}
	// Re-apply same sample.
	if got := e.Update(3); !almost(got, 2.25) {
		t.Fatalf("re-Update(3) = %v, want 2.25", got)
	}
	// Undo down to empty.
	e.Undo(3)
	e.Undo(2)
	e.Undo(1)
	if !math.IsNaN(e.Value()) {
		t.Fatalf("fully undone EMA should be NaN, got %v", e.Value())
	}
	if got := e.Update(10); !almost(got, 10) {
		t.Fatalf("seed after full undo = %v, want 10", got)
	}
}

func TestEMAPeriod1Undo(t *testing.T) {
	// Period 1 (k=1): EMA is always the last sample. Undo must restore the
	// prior sample, not zero.
	e := NewEMA(1)
	e.Update(10)
	e.Update(20)
	if !almost(e.Value(), 20) {
		t.Fatalf("period-1 after second Update = %v, want 20", e.Value())
	}
	e.Undo(20)
	if !almost(e.Value(), 10) {
		t.Fatalf("period-1 after Undo = %v, want 10", e.Value())
	}
	e.Undo(10)
	if !math.IsNaN(e.Value()) {
		t.Fatalf("period-1 fully undone should be NaN, got %v", e.Value())
	}
}

func TestVWAP(t *testing.T) {
	var v VWAP
	if !math.IsNaN(v.Value()) {
		t.Fatal("VWAP should be NaN before any volume")
	}
	v.Update(100, 10) // pv=1000
	v.Update(102, 20) // pv=3040, vol=30
	if got := v.Value(); !almost(got, 3040.0/30.0) {
		t.Fatalf("VWAP = %v, want %v", got, 3040.0/30.0)
	}
	p := v.Peek(110, 30)
	if !almost(p, (3040.0+3300.0)/60.0) {
		t.Fatalf("Peek = %v", p)
	}
	if !almost(v.Value(), 3040.0/30.0) {
		t.Fatal("Peek mutated state")
	}
	v.Reset()
	if !math.IsNaN(v.Value()) {
		t.Fatal("VWAP should be NaN after Reset")
	}
}
