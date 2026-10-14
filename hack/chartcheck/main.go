// Command chartcheck fails when the Helm chart renders other objects than the
// Kustomize install. Labels and selectors differ by design and are not compared;
// instead each selector is checked to match its pods.
//
//	go run ./hack/chartcheck -kustomize dist/install.yaml,dist/config-controller.yaml -helm rendered.yaml
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

type object = map[string]any

const deployment = "Deployment"

func main() {
	kustomize := flag.String("kustomize", "dist/install.yaml,dist/config-controller.yaml",
		"comma-separated Kustomize renders")
	helm := flag.String("helm", "", "helm template output")
	flag.Parse()

	problems, err := check(strings.Split(*kustomize, ","), *helm)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(2)
	}

	for _, p := range problems {
		fmt.Println(p)
	}

	if len(problems) > 0 {
		fmt.Printf("%d difference(s) between the chart and the Kustomize installers\n", len(problems))
		os.Exit(1)
	}

	fmt.Println("chart and Kustomize installers render the same objects")
}

func check(kustomizeFiles []string, helmFile string) ([]string, error) {
	want := map[string]object{}

	for _, f := range kustomizeFiles {
		if err := load(f, want); err != nil {
			return nil, err
		}
	}

	got := map[string]object{}
	if err := load(helmFile, got); err != nil {
		return nil, err
	}

	problems := selectorProblems(got)

	// the chart does not create the namespace
	for k := range want {
		if strings.HasPrefix(k, "Namespace/") {
			delete(want, k)
		}
	}

	for _, k := range sortedKeys(want) {
		g, ok := got[k]
		if !ok {
			problems = append(problems, "missing from the chart: "+k)

			continue
		}

		w := normalize(want[k])
		g = normalize(g)

		for _, d := range diff(w, g, "") {
			problems = append(problems, k+": "+d)
		}
	}

	for _, k := range sortedKeys(got) {
		if _, ok := want[k]; !ok {
			problems = append(problems, "only in the chart: "+k)
		}
	}

	return problems, nil
}

func load(path string, into map[string]object) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		var o object
		if err := dec.Decode(&o); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("%s: %w", path, err)
		}

		if len(o) == 0 {
			continue
		}

		meta, _ := o["metadata"].(object)
		ns, _ := meta["namespace"].(string)
		key := fmt.Sprintf("%s/%s/%s/%s", o["kind"], o["apiVersion"], ns, meta["name"])
		into[key] = o
	}
}

func normalize(o object) object {
	o = deepCopy(o)

	if meta, ok := o["metadata"].(object); ok {
		delete(meta, "labels")
		delete(meta, "annotations")
		delete(meta, "creationTimestamp")
	}

	delete(o, "status")

	spec, _ := o["spec"].(object)

	switch o["kind"] {
	case "Service":
		delete(spec, "selector")
	case deployment:
		delete(spec, "selector")

		if s, ok := spec["strategy"].(object); ok && len(s) == 0 {
			delete(spec, "strategy")
		}

		tmpl, _ := spec["template"].(object)
		if meta, ok := tmpl["metadata"].(object); ok {
			delete(meta, "labels")
			delete(meta, "creationTimestamp")
		}

		pod, _ := tmpl["spec"].(object)
		delete(pod, "affinity")

		for _, c := range list(pod["containers"]) {
			if env, ok := c.(object)["env"].([]any); ok {
				sort.Slice(env, func(i, j int) bool {
					return fmt.Sprint(env[i].(object)["name"]) < fmt.Sprint(env[j].(object)["name"])
				})
			}
		}
	}

	return o
}

// selectorProblems: each Deployment selects its own pods, each Service one
// Deployment's, and the anti-affinity covers every controller pod.
func selectorProblems(objs map[string]object) []string {
	var problems []string

	pods := map[string]map[string]any{}

	for k, o := range objs {
		if o["kind"] != deployment {
			continue
		}

		spec, _ := o["spec"].(object)
		tmpl, _ := spec["template"].(object)
		labels := path(tmpl, "metadata", "labels")
		pods[k] = labels

		if !matches(path(spec, "selector", "matchLabels"), labels) {
			problems = append(problems, k+": selector does not match its own pod labels")
		}
	}

	for k, o := range objs {
		switch o["kind"] {
		case "Service":
			sel := path(o, "spec", "selector")

			n := 0

			for _, labels := range pods {
				if matches(sel, labels) {
					n++
				}
			}

			if n != 1 {
				problems = append(problems, fmt.Sprintf("%s: selector matches %d Deployments, want 1", k, n))
			}
		case deployment:
			anti := path(o, "spec", "template", "spec", "affinity", "podAntiAffinity")

			terms := list(anti["requiredDuringSchedulingIgnoredDuringExecution"])
			if len(terms) == 0 {
				problems = append(problems, k+": no pod anti-affinity")

				continue
			}

			sel := path(terms[0].(object), "labelSelector", "matchLabels")
			for other, labels := range pods {
				if !matches(sel, labels) {
					problems = append(problems, k+": anti-affinity does not cover "+other)
				}
			}
		}
	}

	sort.Strings(problems)

	return problems
}

func matches(selector, labels map[string]any) bool {
	if len(selector) == 0 {
		return false
	}

	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}

	return true
}

func diff(want, got any, at string) []string {
	if reflect.DeepEqual(want, got) {
		return nil
	}

	w, wok := want.(object)
	g, gok := got.(object)

	if wok && gok {
		var out []string

		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}

		for k := range g {
			keys[k] = true
		}

		for _, k := range sortedKeys(keys) {
			out = append(out, diff(w[k], g[k], at+"."+k)...)
		}

		return out
	}

	wl, wlok := want.([]any)
	gl, glok := got.([]any)

	if wlok && glok && len(wl) == len(gl) {
		var out []string
		for i := range wl {
			out = append(out, diff(wl[i], gl[i], fmt.Sprintf("%s[%d]", at, i))...)
		}

		return out
	}

	return []string{fmt.Sprintf("%s: kustomize %s, chart %s", at, compact(want), compact(got))}
}

func compact(v any) string {
	if v == nil {
		return "<absent>"
	}

	b, _ := json.Marshal(v)
	if len(b) > 160 {
		return string(b[:160]) + "..."
	}

	return string(b)
}

func path(o object, keys ...string) map[string]any {
	cur := o
	for _, k := range keys {
		next, _ := cur[k].(object)
		cur = next
	}

	return cur
}

func list(v any) []any {
	l, _ := v.([]any)

	return l
}

func deepCopy(o object) object {
	b, _ := json.Marshal(o)

	var out object
	_ = json.Unmarshal(b, &out)

	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}
