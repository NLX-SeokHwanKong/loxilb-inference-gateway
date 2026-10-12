package models

import (
	"github.com/go-openapi/strfmt"
	"testing"
)

func TestEndPointHostStateTupleValidation(t *testing.T) {
	for _, value := range []EndPointHostState{
		{}, {EpPort: 0}, {EpPort: 1, EpProto: "tcp"}, {EpPort: 65535, EpProto: "tcp"},
		{EpPort: 443, EpProto: "https"}, {EpPort: 443, EpProto: "tls-hello"},
	} {
		if err := value.Validate(strfmt.Default); err != nil {
			t.Fatalf("valid tuple %+v: %v", value, err)
		}
	}
	for _, value := range []EndPointHostState{
		{EpPort: -1, EpProto: "tcp"}, {EpPort: 65536, EpProto: "tcp"},
		{EpPort: 4294967297, EpProto: "tcp"}, {EpPort: 1}, {EpProto: "tcp"},
		{EpPort: 8201, EpProto: "unknown"},
	} {
		if err := value.Validate(strfmt.Default); err == nil {
			t.Fatalf("accepted invalid tuple %+v", value)
		}
	}
	var missing *EndPointHostState
	if err := missing.Validate(strfmt.Default); err == nil {
		t.Fatal("accepted missing payload")
	}
}
