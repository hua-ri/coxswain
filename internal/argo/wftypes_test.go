package argo

import "testing"

func TestPodName(t *testing.T) {
	var w Workflow
	w.Metadata.Name = "hello-xyz"
	// v2 is <workflow>-<template>-<fnv32a(node name)>; the hash was
	// computed independently.
	n := WFNode{ID: "hello-xyz-123", Name: "hello-xyz[0].step", TemplateName: "whalesay", Type: "Pod"}
	if got := w.PodName(n); got != "hello-xyz-whalesay-347245399" {
		t.Fatalf("v2 = %s", got)
	}
	if w.PodName(WFNode{Name: "hello-xyz", Type: "Pod"}) != "hello-xyz" {
		t.Fatal("root pod")
	}
	w.Metadata.Annotations = map[string]string{"workflows.argoproj.io/pod-name-format": "v1"}
	if w.PodName(n) != "hello-xyz-123" {
		t.Fatal("v1")
	}
	if w.PodName(WFNode{Type: "Steps"}) != "" {
		t.Fatal("non-pod node")
	}
}
