package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metavalidation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// validateScheduling checks selector syntax before a full-stop migration. Kubernetes owns the
// remaining affinity rules, and only the scheduler can determine whether placement is satisfiable.
func (c *Cosmosigner) validateScheduling(path string) error {
	p := field.NewPath(path)
	errs := []error{metavalidation.ValidateLabels(c.NodeSelector, p.Child("nodeSelector")).ToAggregate()}
	if c.Affinity == nil {
		return utilerrors.NewAggregate(errs)
	}
	operators := map[corev1.NodeSelectorOperator]selection.Operator{
		corev1.NodeSelectorOpIn: selection.In, corev1.NodeSelectorOpNotIn: selection.NotIn,
		corev1.NodeSelectorOpExists: selection.Exists, corev1.NodeSelectorOpDoesNotExist: selection.DoesNotExist,
		corev1.NodeSelectorOpGt: selection.GreaterThan, corev1.NodeSelectorOpLt: selection.LessThan,
	}
	validateWeight := func(weight int32, path *field.Path) {
		if weight < 1 || weight > 100 {
			errs = append(errs, field.Invalid(path, weight, "must be in the range 1-100"))
		}
	}
	validateNodeTerm := func(term corev1.NodeSelectorTerm, path *field.Path) {
		for i, req := range term.MatchExpressions {
			rp := path.Child("matchExpressions").Index(i)
			op, ok := operators[req.Operator]
			if !ok {
				errs = append(errs, field.NotSupported(rp.Child("operator"), req.Operator, []string{"In", "NotIn", "Exists", "DoesNotExist", "Gt", "Lt"}))
				continue
			}
			if _, err := labels.NewRequirement(req.Key, op, req.Values, field.WithPath(rp)); err != nil {
				errs = append(errs, err)
			}
		}
		for i, req := range term.MatchFields {
			errs = append(errs, metavalidation.ValidateFieldSelectorRequirement(metav1.FieldSelectorRequirement{
				Key: req.Key, Operator: metav1.FieldSelectorOperator(req.Operator), Values: req.Values,
			}, metavalidation.FieldSelectorValidationOptions{}, path.Child("matchFields").Index(i)).ToAggregate())
		}
	}
	if a := c.Affinity.NodeAffinity; a != nil {
		ap := p.Child("affinity", "nodeAffinity")
		if a.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			for i, term := range a.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
				validateNodeTerm(term, ap.Child("requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms").Index(i))
			}
		}
		for i, term := range a.PreferredDuringSchedulingIgnoredDuringExecution {
			tp := ap.Child("preferredDuringSchedulingIgnoredDuringExecution").Index(i)
			validateWeight(term.Weight, tp.Child("weight"))
			validateNodeTerm(term.Preference, tp.Child("preference"))
		}
	}
	validatePodTerm := func(term corev1.PodAffinityTerm, path *field.Path) {
		opts := metavalidation.LabelSelectorValidationOptions{}
		errs = append(errs, metavalidation.ValidateLabelSelector(term.LabelSelector, opts, path.Child("labelSelector")).ToAggregate())
		errs = append(errs, metavalidation.ValidateLabelSelector(term.NamespaceSelector, opts, path.Child("namespaceSelector")).ToAggregate())
		errs = append(errs, metavalidation.ValidateLabelName(term.TopologyKey, path.Child("topologyKey")).ToAggregate())
		for i, namespace := range term.Namespaces {
			for _, msg := range validation.IsDNS1123Label(namespace) {
				errs = append(errs, field.Invalid(path.Child("namespaces").Index(i), namespace, msg))
			}
		}
		for i, key := range term.MatchLabelKeys {
			errs = append(errs, metavalidation.ValidateLabelName(key, path.Child("matchLabelKeys").Index(i)).ToAggregate())
		}
		for i, key := range term.MismatchLabelKeys {
			errs = append(errs, metavalidation.ValidateLabelName(key, path.Child("mismatchLabelKeys").Index(i)).ToAggregate())
		}
	}
	for _, a := range []struct {
		name      string
		required  []corev1.PodAffinityTerm
		preferred []corev1.WeightedPodAffinityTerm
	}{
		{name: "podAffinity"},
		{name: "podAntiAffinity"},
	} {
		if a.name == "podAffinity" && c.Affinity.PodAffinity != nil {
			a.required = c.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			a.preferred = c.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution
		}
		if a.name == "podAntiAffinity" && c.Affinity.PodAntiAffinity != nil {
			a.required = c.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			a.preferred = c.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
		}
		ap := p.Child("affinity", a.name)
		for i, term := range a.required {
			validatePodTerm(term, ap.Child("requiredDuringSchedulingIgnoredDuringExecution").Index(i))
		}
		for i, term := range a.preferred {
			tp := ap.Child("preferredDuringSchedulingIgnoredDuringExecution").Index(i)
			validateWeight(term.Weight, tp.Child("weight"))
			validatePodTerm(term.PodAffinityTerm, tp.Child("podAffinityTerm"))
		}
	}
	return utilerrors.NewAggregate(errs)
}
