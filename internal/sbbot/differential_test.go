package sbbot

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestPythonOutboundCompatibility(t *testing.T) {
	raw, err := os.ReadFile("testdata/python-outbounds.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Input    string
		Expected Doc
	}
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		got, err := ParseOutbound(tc.Input)
		if err != nil {
			t.Fatal(err)
		}
		normalized, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var actual Doc
		if err = json.Unmarshal(normalized, &actual); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, tc.Expected) {
			t.Errorf("%s: got %#v; Python %#v", tc.Input, actual, tc.Expected)
		}
	}
}
