package httpapi

import (
	"context"
	"github.com/getkin/kin-openapi/openapi3"
	"testing"
)

func TestOpenAPI(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
}
