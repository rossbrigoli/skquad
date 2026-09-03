package v1_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	apimachineryyaml "k8s.io/apimachinery/pkg/util/yaml"
	apiservalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
)

// crdDir resolves the Helm chart CRD directory from this package. The chart is
// the single source of truth for what the API server will actually accept, so
// these tests deliberately reach into it instead of restating the schema.
func crdDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "charts", "skquad", "crds"))
	if err != nil {
		t.Fatalf("resolve crd dir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("chart crd dir not found (%s): %v", dir, err)
	}
	return dir
}

func loadCRD(t *testing.T, file string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(crdDir(t), file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	dec := apimachineryyaml.NewYAMLToJSONDecoder(bytes.NewReader(raw))
	var crd apiextensionsv1.CustomResourceDefinition
	if err := dec.Decode(&crd); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	return &crd
}

func TestGroupVersionConstants(t *testing.T) {
	if skquadv1.Group != "skquad.io" {
		t.Errorf("Group = %q, want skquad.io", skquadv1.Group)
	}
	if skquadv1.Version != "v1" {
		t.Errorf("Version = %q, want v1", skquadv1.Version)
	}
	want := schema.GroupVersion{Group: "skquad.io", Version: "v1"}
	if skquadv1.GroupVersion != want {
		t.Errorf("GroupVersion = %+v, want %+v", skquadv1.GroupVersion, want)
	}
}

func TestSchemeRegistration(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	for _, obj := range []runtime.Object{&skquadv1.Squad{}, &skquadv1.SquadList{}, &skquadv1.Agent{}, &skquadv1.AgentList{}} {
		gvks, unstructured, err := scheme.ObjectKinds(obj)
		if err != nil {
			t.Fatalf("ObjectKinds(%T): %v", obj, err)
		}
		if unstructured {
			t.Errorf("ObjectKinds(%T) reported unstructured", obj)
		}
		if len(gvks) != 1 {
			t.Fatalf("ObjectKinds(%T) = %v, want exactly one gvk", obj, gvks)
		}
		if gvks[0].GroupVersion() != skquadv1.GroupVersion {
			t.Errorf("ObjectKinds(%T) gvk = %s, want %s", obj, gvks[0], skquadv1.GroupVersion.WithKind(gvks[0].Kind))
		}
	}

	if !scheme.Recognizes(skquadv1.GroupVersion.WithKind("Squad")) {
		t.Error("scheme does not recognize skquad.io/v1 Squad")
	}
	if !scheme.Recognizes(skquadv1.GroupVersion.WithKind("AgentList")) {
		t.Error("scheme does not recognize skquad.io/v1 AgentList")
	}
}

func TestAddToSchemeIsIdempotent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("first AddToScheme: %v", err)
	}
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("second AddToScheme: %v", err)
	}
}

// TestMetaTypesRegistered asserts metav1.AddToGroupVersion ran, which is what
// lets client-go encode Get/List/Delete options against skquad.io.
func TestMetaTypesRegistered(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	gvk := schema.GroupVersionKind{Group: skquadv1.Group, Version: skquadv1.Version, Kind: "ListOptions"}
	if !scheme.Recognizes(gvk) {
		t.Errorf("scheme does not recognize %s; metav1.AddToGroupVersion likely missing", gvk)
	}
}

// ---------------------------------------------------------------------------
// DeepCopy isolation. Aliasing here means a reconciler mutating its own copy
// silently mutates the informer cache, which is a classic controller-runtime
// footgun, so the raw JSON payloads and condition slices must be copied.
// ---------------------------------------------------------------------------

func TestSquadDeepCopyIsolation(t *testing.T) {
	original := &skquadv1.Squad{}
	original.SetName("squad-a")
	original.SetLabels(map[string]string{"team": "platform"})
	original.Spec.OperatingModel = apiextensionsv1.JSON{Raw: []byte(`{"cadence":"daily"}`)}
	original.Status.Conditions = conditions()

	copied := original.DeepCopyObject().(*skquadv1.Squad)

	if reflect.DeepEqual(original, copied) && &original.Spec == &copied.Spec {
		t.Fatal("DeepCopyObject returned the same pointer")
	}

	// Mutate the copy's raw payload bytes in place: the original must not move.
	copied.Spec.OperatingModel.Raw[0] = 'X'
	if string(original.Spec.OperatingModel.Raw) != `{"cadence":"daily"}` {
		t.Errorf("mutating copy corrupted original OperatingModel: %s", original.Spec.OperatingModel.Raw)
	}

	copied.Status.Conditions[0].Reason = "Changed"
	if original.Status.Conditions[0].Reason == "Changed" {
		t.Error("mutating copy Status.Conditions corrupted original (aliased slice)")
	}

	labels := copied.GetLabels()
	labels["team"] = "changed"
	if original.GetLabels()["team"] != "platform" {
		t.Error("mutating copy labels corrupted original (aliased map)")
	}
}

func TestAgentDeepCopyIsolation(t *testing.T) {
	original := &skquadv1.Agent{}
	original.SetName("agent-a")
	original.Spec.Permissions = apiextensionsv1.JSON{Raw: []byte(`{"scopes":["tasks:read"]}`)}
	original.Status.Conditions = conditions()

	copied := original.DeepCopyObject().(*skquadv1.Agent)

	copied.Spec.Permissions.Raw[0] = 'X'
	if string(original.Spec.Permissions.Raw) != `{"scopes":["tasks:read"]}` {
		t.Errorf("mutating copy corrupted original Permissions: %s", original.Spec.Permissions.Raw)
	}

	copied.Status.Conditions[0].Message = "changed"
	if original.Status.Conditions[0].Message == "changed" {
		t.Error("mutating copy Status.Conditions corrupted original (aliased slice)")
	}
}

func TestListDeepCopiesAreIsolated(t *testing.T) {
	squad := &skquadv1.Squad{}
	squad.SetName("squad-a")
	squad.Spec.OperatingModel = apiextensionsv1.JSON{Raw: []byte(`{"a":1}`)}
	squadList := &skquadv1.SquadList{Items: []skquadv1.Squad{*squad}}

	copiedSquads := squadList.DeepCopyObject().(*skquadv1.SquadList)
	copiedSquads.Items[0].Spec.OperatingModel.Raw[1] = 'Z'
	if string(squad.Spec.OperatingModel.Raw) != `{"a":1}` {
		t.Errorf("SquadList deep copy aliased item payload: %s", squad.Spec.OperatingModel.Raw)
	}

	agent := &skquadv1.Agent{}
	agent.SetName("agent-a")
	agent.Spec.Permissions = apiextensionsv1.JSON{Raw: []byte(`{"b":2}`)}
	agentList := &skquadv1.AgentList{Items: []skquadv1.Agent{*agent}}

	copiedAgents := agentList.DeepCopyObject().(*skquadv1.AgentList)
	copiedAgents.Items[0].Spec.Permissions.Raw[1] = 'Z'
	if string(agent.Spec.Permissions.Raw) != `{"b":2}` {
		t.Errorf("AgentList deep copy aliased item payload: %s", agent.Spec.Permissions.Raw)
	}
}

func TestDeepCopyObjectNilReceivers(t *testing.T) {
	// Generated DeepCopyObject implementations must tolerate nil so that
	// typed-client nil returns do not panic.
	if (*skquadv1.Squad)(nil).DeepCopyObject() != nil {
		t.Error("(*Squad)(nil).DeepCopyObject() should be nil")
	}
	if (*skquadv1.SquadList)(nil).DeepCopyObject() != nil {
		t.Error("(*SquadList)(nil).DeepCopyObject() should be nil")
	}
	if (*skquadv1.Agent)(nil).DeepCopyObject() != nil {
		t.Error("(*Agent)(nil).DeepCopyObject() should be nil")
	}
	if (*skquadv1.AgentList)(nil).DeepCopyObject() != nil {
		t.Error("(*AgentList)(nil).DeepCopyObject() should be nil")
	}
}

// ---------------------------------------------------------------------------
// CRD schema behaviour.
// ---------------------------------------------------------------------------

func TestCRDsAreStructurallyValid(t *testing.T) {
	for _, file := range []string{"skquad.io_squads.yaml", "skquad.io_agents.yaml"} {
		t.Run(file, func(t *testing.T) {
			crd := loadCRD(t, file)
			validateStructural(t, crd.Spec.Versions[0].Schema.OpenAPIV3Schema)
		})
	}
}

// TestCRDSingleServedVersion pins the conversion surface: there is exactly one
// version and it is both served and storage, so no conversion webhook is
// required. If a second version is ever added this test must be replaced by
// real round-trip conversion tests, not deleted silently.
func TestCRDSingleServedVersion(t *testing.T) {
	for _, file := range []string{"skquad.io_squads.yaml", "skquad.io_agents.yaml"} {
		t.Run(file, func(t *testing.T) {
			crd := loadCRD(t, file)
			if len(crd.Spec.Versions) != 1 {
				t.Fatalf("versions = %d, want exactly 1; a second version requires conversion tests", len(crd.Spec.Versions))
			}
			v := crd.Spec.Versions[0]
			if v.Name != skquadv1.Version {
				t.Errorf("version name = %q, want %q", v.Name, skquadv1.Version)
			}
			if !v.Served {
				t.Error("version is not served")
			}
			if !v.Storage {
				t.Error("version is not the storage version")
			}
			if crd.Spec.Group != skquadv1.Group {
				t.Errorf("group = %q, want %q", crd.Spec.Group, skquadv1.Group)
			}
			if string(crd.Spec.Scope) != "Namespaced" {
				t.Errorf("scope = %q, want Namespaced", crd.Spec.Scope)
			}
			if v.Subresources == nil || v.Subresources.Status == nil {
				t.Error("status subresource not declared; spec/status split will not be enforced by the API server")
			}
		})
	}
}

// TestGoTypesMatchCRDProperties catches drift between the Go structs the
// operator reconciles and the schema the API server will accept. A field added
// to the Go type but not the CRD is silently pruned on write; a field in the
// CRD with a different JSON name never lands in the Go struct.
func TestGoTypesMatchCRDProperties(t *testing.T) {
	cases := []struct {
		file     string
		kind     string
		specType reflect.Type
	}{
		{"skquad.io_squads.yaml", "Squad", reflect.TypeOf(skquadv1.SquadSpec{})},
		{"skquad.io_agents.yaml", "Agent", reflect.TypeOf(skquadv1.AgentSpec{})},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			crd := loadCRD(t, tc.file)
			if crd.Spec.Names.Kind != tc.kind {
				t.Fatalf("CRD kind = %q, want %q", crd.Spec.Names.Kind, tc.kind)
			}
			schemaProps := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties

			goFields := jsonFieldNames(tc.specType)

			missingInCRD := []string{}
			for name := range goFields {
				if _, ok := schemaProps[name]; !ok {
					missingInCRD = append(missingInCRD, name)
				}
			}
			if len(missingInCRD) > 0 {
				sort.Strings(missingInCRD)
				t.Errorf("Go %sSpec fields absent from CRD schema (they will be pruned by the API server): %v", tc.kind, missingInCRD)
			}

			missingInGo := []string{}
			for name := range schemaProps {
				if _, ok := goFields[name]; !ok {
					missingInGo = append(missingInGo, name)
				}
			}
			if len(missingInGo) > 0 {
				sort.Strings(missingInGo)
				t.Errorf("CRD schema properties absent from Go %sSpec (they will be silently dropped): %v", tc.kind, missingInGo)
			}
		})
	}
}

// TestCRDRequiredFieldsAreNotOmitempty asserts the CRD never requires a field
// the Go type would omit from serialization, which would make every object the
// operator writes invalid.
func TestCRDRequiredFieldsAreNotOmitempty(t *testing.T) {
	cases := []struct {
		file     string
		specType reflect.Type
	}{
		{"skquad.io_squads.yaml", reflect.TypeOf(skquadv1.SquadSpec{})},
		{"skquad.io_agents.yaml", reflect.TypeOf(skquadv1.AgentSpec{})},
	}
	for _, tc := range cases {
		t.Run(tc.specType.Name(), func(t *testing.T) {
			crd := loadCRD(t, tc.file)
			spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
			fields := jsonFieldNames(tc.specType)
			for _, required := range spec.Required {
				info, ok := fields[required]
				if !ok {
					t.Errorf("CRD requires %q which is not a Go field", required)
					continue
				}
				if info.omitempty {
					t.Errorf("CRD requires %q but the Go field is omitempty; operator can write objects the API server rejects", required)
				}
			}
		})
	}
}

// TestCustomResourceValidation exercises the actual schema the API server uses:
// required fields, type enforcement, and free-form payloads.
func TestCustomResourceValidation(t *testing.T) {
	agentCRD := loadCRD(t, "skquad.io_agents.yaml")
	validator, err := newResourceValidator(agentCRD)
	if err != nil {
		t.Fatalf("build validator: %v", err)
	}

	tests := []struct {
		name      string
		object    string
		wantError string
	}{
		{
			name: "valid minimal agent",
			object: `{
				"apiVersion": "skquad.io/v1",
				"kind": "Agent",
				"metadata": {"name": "agent-a"},
				"spec": {"agentId": "a-1", "squadId": "s-1", "desiredActive": true}
			}`,
		},
		{
			name: "valid agent with free-form permissions",
			object: `{
				"apiVersion": "skquad.io/v1",
				"kind": "Agent",
				"metadata": {"name": "agent-a"},
				"spec": {
					"agentId": "a-1", "squadId": "s-1", "desiredActive": false,
					"permissions": {"nested": {"deep": [1, 2, 3]}, "scopes": ["tasks:write"]}
				}
			}`,
		},
		{
			name: "missing agentId",
			object: `{
				"apiVersion": "skquad.io/v1",
				"kind": "Agent",
				"metadata": {"name": "agent-a"},
				"spec": {"squadId": "s-1", "desiredActive": true}
			}`,
			wantError: "agentId",
		},
		{
			name: "missing squadId",
			object: `{
				"apiVersion": "skquad.io/v1",
				"kind": "Agent",
				"metadata": {"name": "agent-a"},
				"spec": {"agentId": "a-1", "desiredActive": true}
			}`,
			wantError: "squadId",
		},
		{
			name: "missing desiredActive",
			object: `{
				"apiVersion": "skquad.io/v1",
				"kind": "Agent",
				"metadata": {"name": "agent-a"},
				"spec": {"agentId": "a-1", "squadId": "s-1"}
			}`,
			wantError: "desiredActive",
		},
		{
			name: "missing spec",
			object: `{
				"apiVersion": "skquad.io/v1",
				"kind": "Agent",
				"metadata": {"name": "agent-a"}
			}`,
			wantError: "spec",
		},
		{
			name: "desiredActive wrong type",
			object: `{
				"apiVersion": "skquad.io/v1",
				"kind": "Agent",
				"metadata": {"name": "agent-a"},
				"spec": {"agentId": "a-1", "squadId": "s-1", "desiredActive": "yes"}
			}`,
			wantError: "Invalid value",
		},
		{
			name: "replicas wrong type",
			object: `{
				"apiVersion": "skquad.io/v1",
				"kind": "Agent",
				"metadata": {"name": "agent-a"},
				"spec": {"agentId": "a-1", "squadId": "s-1", "desiredActive": true},
				"status": {"replicas": "three"}
			}`,
			wantError: "Invalid value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var obj map[string]interface{}
			if err := json.Unmarshal([]byte(tc.object), &obj); err != nil {
				t.Fatalf("unmarshal test object: %v", err)
			}
			errs := apiservalidation.ValidateCustomResource(field.NewPath(""), obj, validator)
			if tc.wantError == "" {
				if len(errs) > 0 {
					t.Fatalf("unexpected validation errors: %v", errs.ToAggregate())
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("expected validation error mentioning %q, got none", tc.wantError)
			}
			msg := errs.ToAggregate().Error()
			if !strings.Contains(msg, tc.wantError) {
				t.Errorf("expected error to mention %q, got: %s", tc.wantError, msg)
			}
		})
	}
}

// TestSquadCustomResourceValidation covers the Squad schema required pair and
// the free-form operatingModel payload.
func TestSquadCustomResourceValidation(t *testing.T) {
	crd := loadCRD(t, "skquad.io_squads.yaml")
	validator, err := newResourceValidator(crd)
	if err != nil {
		t.Fatalf("build validator: %v", err)
	}

	valid := `{
		"apiVersion": "skquad.io/v1",
		"kind": "Squad",
		"metadata": {"name": "squad-a"},
		"spec": {
			"squadId": "s-1", "ownerRef": "user:ross",
			"operatingModel": {"roles": {"lead": 1}, "cadence": ["daily","weekly"]}
		}
	}`
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(valid), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if errs := apiservalidation.ValidateCustomResource(field.NewPath(""), obj, validator); len(errs) > 0 {
		t.Fatalf("valid Squad rejected: %v", errs.ToAggregate())
	}

	for _, missing := range []string{"squadId", "ownerRef"} {
		broken := map[string]interface{}{
			"apiVersion": "skquad.io/v1",
			"kind":       "Squad",
			"metadata":   map[string]interface{}{"name": "squad-a"},
			"spec":       map[string]interface{}{"squadId": "s-1", "ownerRef": "user:ross"},
		}
		spec := broken["spec"].(map[string]interface{})
		delete(spec, missing)
		errs := apiservalidation.ValidateCustomResource(field.NewPath(""), broken, validator)
		if len(errs) == 0 {
			t.Errorf("expected rejection when %q is missing", missing)
			continue
		}
		if !strings.Contains(errs.ToAggregate().Error(), missing) {
			t.Errorf("error for missing %q does not name the field: %v", missing, errs.ToAggregate())
		}
	}
}

// TestPrinterColumnsResolveAgainstGoTypes keeps the CRD additionalPrinterColumns
// honest: a jsonPath that no longer matches a Go json tag shows blank columns in
// kubectl output rather than failing loudly.
func TestPrinterColumnsResolveAgainstGoTypes(t *testing.T) {
	cases := []struct {
		file     string
		rootType reflect.Type
	}{
		{"skquad.io_squads.yaml", reflect.TypeOf(skquadv1.Squad{})},
		{"skquad.io_agents.yaml", reflect.TypeOf(skquadv1.Agent{})},
	}
	for _, tc := range cases {
		t.Run(tc.rootType.Name(), func(t *testing.T) {
			crd := loadCRD(t, tc.file)
			for _, col := range crd.Spec.Versions[0].AdditionalPrinterColumns {
				path := strings.TrimPrefix(col.JSONPath, ".")
				if !jsonPathExists(tc.rootType, strings.Split(path, ".")) {
					t.Errorf("printer column %q jsonPath %s does not resolve on Go type %s", col.Name, col.JSONPath, tc.rootType)
				}
			}
		})
	}
}

// TestDecodeRoundTripPreservesSpec decodes manifests through the registered
// scheme and re-encodes them. For a single-version API this round trip is the
// whole conversion surface; losing the free-form JSON payloads here would break
// grants and operating models on every reconcile.
func TestDecodeRoundTripPreservesSpec(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	const agentManifest = `apiVersion: skquad.io/v1
kind: Agent
metadata:
  name: agent-a
spec:
  agentId: a-1
  squadId: s-1
  image: ghcr.io/rossbrigoli/skquad-agent-runtime:latest
  desiredActive: true
  permissions:
    scopes:
      - tasks:read
      - tasks:write
`

	codec := serializer.NewCodecFactory(scheme).UniversalDeserializer()
	decoded, gvk, err := codec.Decode([]byte(agentManifest), nil, nil)
	if err != nil {
		t.Fatalf("decode agent manifest: %v", err)
	}
	if gvk == nil || gvk.Kind != "Agent" || gvk.Version != "v1" {
		t.Fatalf("decoded gvk = %+v, want Agent/v1", gvk)
	}
	agent, ok := decoded.(*skquadv1.Agent)
	if !ok {
		t.Fatalf("decoded type = %T, want *v1.Agent", decoded)
	}
	if agent.Spec.AgentID != "a-1" || agent.Spec.SquadID != "s-1" {
		t.Errorf("identity fields lost on decode: %+v", agent.Spec)
	}
	if !agent.Spec.DesiredActive {
		t.Error("desiredActive lost on decode")
	}
	if agent.Spec.Image == "" {
		t.Error("image lost on decode")
	}
	var perms map[string][]string
	if err := json.Unmarshal(agent.Spec.Permissions.Raw, &perms); err != nil {
		t.Fatalf("permissions raw not valid JSON (%s): %v", agent.Spec.Permissions.Raw, err)
	}
	if len(perms["scopes"]) != 2 {
		t.Errorf("permissions payload mangled on decode: %v", perms)
	}

	// The API server serialises through the same json tags, so a plain marshal
	// round trip is what a write/read cycle against etcd would do.
	out, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("re-encode agent: %v", err)
	}
	var reparsed skquadv1.Agent
	if err := json.Unmarshal(out, &reparsed); err != nil {
		t.Fatalf("reparse encoded agent: %v", err)
	}
	if !reflect.DeepEqual(reparsed.Spec, agent.Spec) {
		t.Errorf("spec changed across encode round trip:\n got %+v\nwant %+v", reparsed.Spec, agent.Spec)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type jsonField struct {
	name      string
	omitempty bool
	typ       reflect.Type
}

func jsonFieldNames(t reflect.Type) map[string]jsonField {
	fields := map[string]jsonField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			continue // inline or untagged
		}
		fields[name] = jsonField{
			name:      name,
			omitempty: strings.Contains(opts, "omitempty"),
			typ:       f.Type,
		}
	}
	return fields
}

func jsonPathExists(t reflect.Type, segments []string) bool {
	for i, seg := range segments {
		for t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return false
		}
		found := false
		for f := 0; f < t.NumField(); f++ {
			name, _, _ := strings.Cut(t.Field(f).Tag.Get("json"), ",")
			if name == seg {
				t = t.Field(f).Type
				found = true
				break
			}
		}
		if !found {
			// metadata is inlined from metav1 and always present.
			if i == 0 && seg == "metadata" {
				return true
			}
			return false
		}
	}
	return true
}
