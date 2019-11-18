// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v2

import (
	"reflect"
	"strconv"
	"testing"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/config/schemas"
)

func TestProxyNeedsPush(t *testing.T) {
	sidecar := &model.Proxy{Type: model.SidecarProxy, IPAddresses: []string{"127.0.0.1"}}
	gateway := &model.Proxy{Type: model.Router}
	cases := []struct {
		name       string
		proxy      *model.Proxy
		namespaces []string
		configs    []string
		want       bool
	}{
		{"no namespace or configs", sidecar, nil, nil, true},
		{"gateway config for sidecar", sidecar, nil, []string{schemas.Gateway.Type}, false},
		{"gateway config for gateway", gateway, nil, []string{schemas.Gateway.Type}, true},
		{"quotaspec config for sidecar", sidecar, nil, []string{schemas.QuotaSpec.Type}, true},
		{"quotaspec config for gateway", gateway, nil, []string{schemas.QuotaSpec.Type}, false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			ns := map[string]struct{}{}
			for _, n := range tt.namespaces {
				ns[n] = struct{}{}
			}
			cfgs := map[string]struct{}{}
			for _, c := range tt.configs {
				cfgs[c] = struct{}{}
			}
			pushEv := &XdsEvent{namespacesUpdated: ns, configTypesUpdated: cfgs}
			got := ProxyNeedsPush(tt.proxy, pushEv)
			if got != tt.want {
				t.Fatalf("Got needs push = %v, expected %v", got, tt.want)
			}
		})
	}
}

func TestPushTypeFor(t *testing.T) {
	t.Parallel()

	sidecar := &model.Proxy{Type: model.SidecarProxy}
	gateway := &model.Proxy{Type: model.Router}

	tests := []struct {
		name        string
		proxy       *model.Proxy
		configTypes []string
		expect      map[XdsType]bool
	}{
		{
			name:        "configTypes is empty",
			proxy:       sidecar,
			configTypes: nil,
			expect:      map[XdsType]bool{CDS: true, EDS: true, LDS: true, RDS: true},
		},
		{
			name:        "configTypes is empty",
			proxy:       gateway,
			configTypes: nil,
			expect:      map[XdsType]bool{CDS: true, EDS: true, LDS: true, RDS: true},
		},
		{
			name:        "sidecar updated for sidecar proxy",
			proxy:       sidecar,
			configTypes: []string{schemas.Sidecar.Type},
			expect:      map[XdsType]bool{CDS: true, EDS: true, LDS: true, RDS: true},
		},
		{
			name:        "sidecar updated for gateway proxy",
			proxy:       gateway,
			configTypes: []string{schemas.Sidecar.Type},
			expect:      map[XdsType]bool{},
		},
		{
			name:        "quotaSpec updated for sidecar proxy",
			proxy:       sidecar,
			configTypes: []string{schemas.QuotaSpec.Type},
			expect:      map[XdsType]bool{LDS: true, RDS: true},
		},
		{
			name:        "quotaSpec updated for gateway",
			proxy:       gateway,
			configTypes: []string{schemas.QuotaSpec.Type},
			expect:      map[XdsType]bool{},
		},
		{
			name:        "authorizationpolicy updated",
			proxy:       sidecar,
			configTypes: []string{schemas.AuthorizationPolicy.Type},
			expect:      map[XdsType]bool{LDS: true},
		},
		{
			name:        "authorizationpolicy updated",
			proxy:       gateway,
			configTypes: []string{schemas.AuthorizationPolicy.Type},
			expect:      map[XdsType]bool{LDS: true},
		},
		{
			name:        "authenticationpolicy updated",
			proxy:       sidecar,
			configTypes: []string{schemas.AuthenticationPolicy.Type},
			expect:      map[XdsType]bool{CDS: true, EDS: true, LDS: true},
		},
		{
			name:        "authenticationpolicy updated",
			proxy:       gateway,
			configTypes: []string{schemas.AuthenticationPolicy.Type},
			expect:      map[XdsType]bool{CDS: true, EDS: true, LDS: true},
		},
		{
			name:        "unknown type updated",
			proxy:       sidecar,
			configTypes: []string{"unknown"},
			expect:      map[XdsType]bool{CDS: true, EDS: true, LDS: true, RDS: true},
		},
		{
			name:        "unknown type updated",
			proxy:       gateway,
			configTypes: []string{},
			expect:      map[XdsType]bool{CDS: true, EDS: true, LDS: true, RDS: true},
		},
		{
			name:        "gateway and virtualservice updated for gateway proxy",
			proxy:       gateway,
			configTypes: []string{schemas.Gateway.Type, schemas.VirtualService.Type},
			expect:      map[XdsType]bool{LDS: true, RDS: true},
		},
		{
			name:        "virtualservice and destinationrule updated",
			proxy:       sidecar,
			configTypes: []string{schemas.DestinationRule.Type, schemas.VirtualService.Type},
			expect:      map[XdsType]bool{CDS: true, EDS: true, LDS: true, RDS: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgs := map[string]struct{}{}
			for _, c := range tt.configTypes {
				cfgs[c] = struct{}{}
			}
			pushEv := &XdsEvent{configTypesUpdated: cfgs}
			out := PushTypeFor(tt.proxy, pushEv)
			if !reflect.DeepEqual(out, tt.expect) {
				t.Errorf("expected: %v, but got %v", tt.expect, out)
			}
		})
	}
}

func BenchmarkListEquals(b *testing.B) {
	size := 100
	var l []string
	for i := 0; i < size; i++ {
		l = append(l, strconv.Itoa(i))
	}
	var equal []string
	for i := 0; i < size; i++ {
		equal = append(equal, strconv.Itoa(i))
	}
	var notEqual []string
	for i := 0; i < size; i++ {
		notEqual = append(notEqual, strconv.Itoa(i))
	}
	notEqual[size-1] = "z"

	for n := 0; n < b.N; n++ {
		listEqualUnordered(l, equal)
		listEqualUnordered(l, notEqual)
	}
}
