// Package v1alpha1 contains the cyphra.dev/v1alpha1 API: Neo4jInstance.
// +kubebuilder:object:generate=true
// +groupName=cyphra.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "cyphra.dev", Version: "v1alpha1"}

	// SchemeBuilder adds the types in this group/version to a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group/version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
