package v1_test

import (
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	apiservalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// toInternalSchema converts a versioned openAPI schema (as written in the chart
// CRD YAML) into the internal apiextensions type the validation packages accept.
// This is the same conversion the API server performs on admission.
func convertSchema(in *apiextensionsv1.JSONSchemaProps) (*apiextensions.JSONSchemaProps, error) {
	out := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(in, out, nil); err != nil {
		return nil, err
	}
	return out, nil
}

func toInternalSchema(t *testing.T, in *apiextensionsv1.JSONSchemaProps) *apiextensions.JSONSchemaProps {
	t.Helper()
	out, err := convertSchema(in)
	if err != nil {
		t.Fatalf("convert schema to internal: %v", err)
	}
	return out
}

// validateStructural asserts the CRD schema is structural: every field typed, no
// free-form objects unless explicitly marked with x-kubernetes-preserve-unknown-fields.
// The API server refuses to install a non-structural CRD, so this fails at deploy
// time if someone hand-edits the chart.
func validateStructural(t *testing.T, schema *apiextensionsv1.JSONSchemaProps) {
	t.Helper()
	structural, err := structuralschema.NewStructural(toInternalSchema(t, schema))
	if err != nil {
		t.Fatalf("schema is not convertible to structural form: %v", err)
	}
	if errs := structuralschema.ValidateStructural(field.NewPath("openAPIV3Schema"), structural); len(errs) > 0 {
		t.Fatalf("CRD schema is not structural:\n%s", errs.ToAggregate())
	}
}

// newResourceValidator builds a validator for objects of the given CRD using the
// storage version's openAPI schema, i.e. exactly what the API server enforces on
// create.
func newResourceValidator(crd *apiextensionsv1.CustomResourceDefinition) (apiservalidation.SchemaValidator, error) {
	internal, err := convertSchema(crd.Spec.Versions[0].Schema.OpenAPIV3Schema)
	if err != nil {
		return nil, err
	}
	validator, _, err := apiservalidation.NewSchemaValidator(internal)
	return validator, err
}

// conditions returns a representative condition set for deep-copy tests.
func conditions() []metav1.Condition {
	return []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            "ok",
		LastTransitionTime: metav1.Now(),
	}}
}
