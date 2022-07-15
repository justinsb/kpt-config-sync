package controllers

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

type ImageRewriter struct {
	RewriteTemplate string
}

func (r *ImageRewriter) RewriteContainer(ctx context.Context, spec *corev1.Container) error {
	tokens := strings.Split(spec.Image, ":")
	if len(tokens) != 2 {
		return fmt.Errorf("unexpected number of colons in image %q", spec.Image)
	}

	imageFQN := tokens[0]
	tag := tokens[1]

	nameTokens := strings.Split(imageFQN, "/")

	name := nameTokens[len(nameTokens)-1]
	family := name // default family to name
	if len(nameTokens) == 3 && nameTokens[0] == "gcr.io" {
		family = nameTokens[1]
	}

	image := r.RewriteTemplate
	if image == "" {
		image = "{{ImageFQN}}"
	}
	image = strings.ReplaceAll(image, "{{ImageFQN}}", imageFQN)
	image = strings.ReplaceAll(image, "{{Tag}}", tag)
	image = strings.ReplaceAll(image, "{{Name}}", name)
	image = strings.ReplaceAll(image, "{{Family}}", family)

	spec.Image = image
	return nil
}
