package catalog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestVendorMappingExtract(t *testing.T) {
	var m VendorMapping
	if err := json.Unmarshal([]byte(`{
		"score": {"path": "$.vendor.rating", "min": 0, "max": 100},
		"on_time_dispatch_rate": "$.vendor.dispatch",
		"response_hours": "$.vendor.hours",
		"verified": "$.vendor.is_verified"
	}`), &m); err != nil {
		t.Fatal(err)
	}
	if errs := ValidateVendorMapping(m); len(errs) > 0 {
		t.Fatal(errs)
	}
	vm, err := NewVendorMapper(m)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := vm.Extract([]byte(`{"vendor": {"rating": 85, "dispatch": "0.95", "hours": 12, "is_verified": true}}`))
	if !ok || *v.Score != 0.85 || *v.OnTimeDispatchRate != 0.95 || *v.ResponseHours != 12 || !*v.Verified || v.DisputeRate != nil {
		t.Errorf("extracted %+v ok=%v", v, ok)
	}
	if v, _ := vm.Extract([]byte(`{"vendor": {"rating": 140}}`)); *v.Score != 1 {
		t.Errorf("rescaled values clamp to [0, 1], got %v", *v.Score)
	}
	if _, ok := vm.Extract([]byte(`{"title": "no vendor block"}`)); ok {
		t.Error("nothing mapped is present")
	}
	if _, ok := vm.Extract([]byte(`not json`)); ok {
		t.Error("bad JSON extracts nothing")
	}

	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), `"on_time_dispatch_rate":"$.vendor.dispatch"`) || !strings.Contains(string(b), `"min":0`) {
		t.Errorf("round trip keeps the short form: %s", b)
	}
	if vm, err := NewVendorMapper(nil); vm != nil || err != nil {
		t.Errorf("no mapping: %v %v", vm, err)
	}
}

func TestValidateVendorMapping(t *testing.T) {
	var m VendorMapping
	err := json.Unmarshal([]byte(`{"score": {"path": "$.s", "minimum": 1}}`), &m)
	if err == nil {
		t.Error("unknown keys in a field object are rejected")
	}
	lo, hi := 10.0, 5.0
	errs := ValidateVendorMapping(VendorMapping{
		"rank":           {Path: "$.r"},
		"score":          {Path: "$.s", Min: &lo, Max: &hi},
		"response_hours": {Path: "$.h", Min: &lo, Max: &lo},
		"dispute_rate":   {Path: "$[bad"},
	})
	want := []string{"/catalog/vendor_mapping/dispute_rate/path", "/catalog/vendor_mapping/rank",
		"/catalog/vendor_mapping/response_hours", "/catalog/vendor_mapping/score"}
	if len(errs) != len(want) {
		t.Fatalf("errors %+v", errs)
	}
	for i, p := range want {
		if errs[i].Path != p {
			t.Errorf("error %d at %s, want %s", i, errs[i].Path, p)
		}
	}
}

func TestVendorValidate(t *testing.T) {
	one, bad, neg := 1.0, 1.2, -2.0
	if errs := (Vendor{ID: "v", Score: &one, ResponseHours: &bad}).Validate(""); len(errs) != 0 {
		t.Errorf("valid vendor: %+v", errs)
	}
	errs := (Vendor{Score: &bad, ResponseHours: &neg}).Validate("/v")
	if len(errs) != 3 || errs[0].Path != "/v/vendor_id" || errs[1].Path != "/v/score" || errs[2].Path != "/v/response_hours" {
		t.Errorf("errors %+v", errs)
	}
	if errs := (Vendor{ID: "v"}).Validate(""); len(errs) != 1 || errs[0].Code != "required" {
		t.Errorf("empty update: %+v", errs)
	}
}

func TestVendorEventRoundTrip(t *testing.T) {
	score := 0.7
	e := NewVendorEvent("t1", Vendor{ID: "v1", Score: &score}, time.Now())
	b, _ := json.Marshal(e)
	got, err := DecodeVendorEvent(b, e.Headers())
	if err != nil || got.TenantID != "t1" || *got.Vendor.Score != 0.7 || string(e.Key()) != "t1:v1" {
		t.Fatalf("got %+v err %v", got, err)
	}
	if _, err := DecodeVendorEvent(b, map[string]string{HeaderTenantID: "t2"}); err == nil {
		t.Error("a header that disagrees with the body is rejected")
	}
	if _, err := DecodeVendorEvent([]byte(`{"tenant_id":"t1","vendor":{"vendor_id":"v1","score":3}}`), nil); err == nil {
		t.Error("out-of-range values are rejected")
	}
	if _, err := DecodeVendorEvent([]byte(`{"vendor":{"vendor_id":"v1","score":0.3}}`), nil); err == nil {
		t.Error("a tenant is required")
	}
}
