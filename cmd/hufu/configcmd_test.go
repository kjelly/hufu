package main

import "testing"

func TestConfigMapValues(t *testing.T) {
	values := map[string]interface{}{}
	setMapValue(values, "profiles.batch.unattended", "true")
	value, ok := mapValue(values, "profiles.batch.unattended")
	if !ok || value != true {
		t.Errorf("got %#v, %t", value, ok)
	}
}
