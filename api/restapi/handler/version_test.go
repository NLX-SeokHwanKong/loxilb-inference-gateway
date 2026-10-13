/*
 * Copyright (c) 2026 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 */
package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/loxilb-io/loxilb/api/models"
	"github.com/loxilb-io/loxilb/api/restapi/operations"
	cmn "github.com/loxilb-io/loxilb/common"
)

// TestVersionAdvertisesFeatures: GET /version carries the product and the
// feature list kube-loxilb gates on, with "epp" among them.
func TestVersionAdvertisesFeatures(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/netlox/v1/version", nil)
	req.URL = &url.URL{Path: "/netlox/v1/version"}
	resp, ok := ConfigGetVersion(operations.GetVersionParams{HTTPRequest: req}).(*operations.GetVersionOK)
	if !ok {
		t.Fatalf("GET /version did not answer 200")
	}
	var got models.VersionGetEntry = *resp.Payload
	if got.Product != cmn.Product {
		t.Errorf("product %q, want %q", got.Product, cmn.Product)
	}
	found := false
	for _, f := range got.Features {
		if f == cmn.FeatureEpp {
			found = true
		}
	}
	if !found {
		t.Errorf("features %v do not include %q", got.Features, cmn.FeatureEpp)
	}
	// The payload is a copy: a client mutating it cannot change what the
	// next caller sees.
	resp.Payload.Features[0] = "mutated"
	if cmn.Features[0] != cmn.FeatureEpp {
		t.Errorf("payload aliases cmn.Features")
	}
}
