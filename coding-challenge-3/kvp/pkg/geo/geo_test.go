package geo

import (
	"math"
	"testing"
)

func f(v float64) *float64 { return &v }

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		in   *PointInput
		ok   bool
	}{
		{"valid", &PointInput{f(10.7769), f(106.7009)}, true},
		{"bounds inclusive", &PointInput{f(-90), f(180)}, true},
		{"lat too high", &PointInput{f(90.0001), f(0)}, false},
		{"lat too low", &PointInput{f(-91), f(0)}, false},
		{"lng too high", &PointInput{f(0), f(180.5)}, false},
		{"lng too low", &PointInput{f(0), f(-181)}, false},
		{"missing lat", &PointInput{nil, f(0)}, false},
		{"missing lng", &PointInput{f(0), nil}, false},
		{"nil", nil, false},
		{"NaN", &PointInput{f(math.NaN()), f(0)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.in.ToPoint()
			if (err == nil) != c.ok {
				t.Fatalf("ok=%v, err=%v", c.ok, err)
			}
		})
	}
}

func TestHaversine(t *testing.T) {
	// Ben Thanh Market → Tan Son Nhat airport ≈ 6.1 km.
	d := HaversineM(Point{10.7725, 106.6980}, Point{10.8185, 106.6588})
	if d < 6000 || d > 6800 {
		t.Fatalf("unexpected distance %f", d)
	}
	if HaversineM(Point{1, 1}, Point{1, 1}) != 0 {
		t.Fatal("same point must be 0")
	}
}
