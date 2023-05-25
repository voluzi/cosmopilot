package chainutils

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type App struct {
	client     *kubernetes.Clientset
	scheme     *runtime.Scheme
	restConfig *rest.Config

	owner      metav1.Object
	binary     string
	image      string
	pullPolicy corev1.PullPolicy
}

func NewApp(client *kubernetes.Clientset, scheme *runtime.Scheme, cfg *rest.Config, owner metav1.Object, options ...Option) *App {
	app := &App{
		client:     client,
		owner:      owner,
		scheme:     scheme,
		restConfig: cfg,
	}
	applyOptions(app, options)
	return app
}

type Option func(*App)

func applyOptions(c *App, options []Option) {
	for _, option := range options {
		option(c)
	}
}

func WithBinary(name string) Option {
	return func(c *App) {
		c.binary = name
	}
}

func WithImage(image string) Option {
	return func(c *App) {
		c.image = image
	}
}

func WithImagePullPolicy(p corev1.PullPolicy) Option {
	return func(c *App) {
		c.pullPolicy = p
	}
}
