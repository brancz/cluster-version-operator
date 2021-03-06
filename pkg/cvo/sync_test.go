package cvo

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/davecgh/go-spew/spew"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	clientgotesting "k8s.io/client-go/testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-version-operator/lib"
	"github.com/openshift/cluster-version-operator/lib/resourcebuilder"
	"github.com/openshift/cluster-version-operator/pkg/cvo/internal"
)

func TestHasRequeueOnErrorAnnotation(t *testing.T) {
	tests := []struct {
		annos map[string]string

		exp     bool
		experrs []string
	}{{
		annos:   nil,
		exp:     false,
		experrs: nil,
	}, {
		annos:   map[string]string{"dummy": "dummy"},
		exp:     false,
		experrs: nil,
	}, {
		annos:   map[string]string{RequeueOnErrorAnnotationKey: "NoMatch"},
		exp:     true,
		experrs: []string{"NoMatch"},
	}, {
		annos:   map[string]string{RequeueOnErrorAnnotationKey: "NoMatch,NotFound"},
		exp:     true,
		experrs: []string{"NoMatch", "NotFound"},
	}}
	for idx, test := range tests {
		t.Run(fmt.Sprintf("test#%d", idx), func(t *testing.T) {
			got, goterrs := hasRequeueOnErrorAnnotation(test.annos)
			if got != test.exp {
				t.Fatalf("expected %v got %v", test.exp, got)
			}
			if !reflect.DeepEqual(goterrs, test.experrs) {
				t.Fatalf("expected %v got %v", test.exp, got)
			}
		})
	}
}

func TestShouldRequeueOnErr(t *testing.T) {
	tests := []struct {
		err      error
		manifest string
		exp      bool
	}{{
		err: nil,
		manifest: `{
			"apiVersion": "v1",
			"kind": "ConfigMap"
		}`,

		exp: false,
	}, {
		err: fmt.Errorf("random error"),
		manifest: `{
			"apiVersion": "v1",
			"kind": "ConfigMap"
		}`,

		exp: false,
	}, {
		err: &meta.NoResourceMatchError{},
		manifest: `{
			"apiVersion": "v1",
			"kind": "ConfigMap"
		}`,

		exp: false,
	}, {
		err: &updateError{cause: &meta.NoResourceMatchError{}},
		manifest: `{
			"apiVersion": "v1",
			"kind": "ConfigMap"
		}`,

		exp: false,
	}, {
		err: &meta.NoResourceMatchError{},
		manifest: `{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"annotations": {
					"v1.cluster-version-operator.operators.openshift.io/requeue-on-error": "NoMatch"
				}
			}
		}`,

		exp: true,
	}, {
		err: &updateError{cause: &meta.NoResourceMatchError{}},
		manifest: `{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"annotations": {
					"v1.cluster-version-operator.operators.openshift.io/requeue-on-error": "NoMatch"
				}
			}
		}`,

		exp: true,
	}, {
		err: &meta.NoResourceMatchError{},
		manifest: `{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"annotations": {
					"v1.cluster-version-operator.operators.openshift.io/requeue-on-error": "NotFound"
				}
			}
		}`,

		exp: false,
	}, {
		err: &updateError{cause: &meta.NoResourceMatchError{}},
		manifest: `{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"annotations": {
					"v1.cluster-version-operator.operators.openshift.io/requeue-on-error": "NotFound"
				}
			}
		}`,

		exp: false,
	}, {
		err: apierrors.NewInternalError(fmt.Errorf("dummy")),
		manifest: `{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"annotations": {
					"v1.cluster-version-operator.operators.openshift.io/requeue-on-error": "NoMatch"
				}
			}
		}`,

		exp: false,
	}, {
		err: &updateError{cause: apierrors.NewInternalError(fmt.Errorf("dummy"))},
		manifest: `{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"annotations": {
					"v1.cluster-version-operator.operators.openshift.io/requeue-on-error": "NoMatch"
				}
			}
		}`,

		exp: false,
	}, {
		err: &updateError{cause: &resourcebuilder.RetryLaterError{}},
		manifest: `{
			"apiVersion": "v1",
			"kind": "ConfigMap"
		}`,

		exp: true,
	}}
	for idx, test := range tests {
		t.Run(fmt.Sprintf("test#%d", idx), func(t *testing.T) {
			var manifest lib.Manifest
			if err := json.Unmarshal([]byte(test.manifest), &manifest); err != nil {
				t.Fatal(err)
			}
			if got := shouldRequeueOnErr(test.err, &manifest); got != test.exp {
				t.Fatalf("expected %v got %v", test.exp, got)
			}
		})
	}
}

func Test_SyncWorker_apply(t *testing.T) {
	tests := []struct {
		manifests []string
		reactors  map[action]error

		check   func(*testing.T, []action)
		wantErr bool
	}{{
		manifests: []string{
			`{
				"apiVersion": "test.cvo.io/v1",
				"kind": "TestA",
				"metadata": {
					"namespace": "default",
					"name": "testa"
				}
			}`,
			`{
				"apiVersion": "test.cvo.io/v1",
				"kind": "TestB",
				"metadata": {
					"namespace": "default",
					"name": "testb"
				}
			}`,
		},
		reactors: map[action]error{},
		check: func(t *testing.T, actions []action) {
			if len(actions) != 2 {
				spew.Dump(actions)
				t.Fatalf("unexpected %d actions", len(actions))
			}

			if got, exp := actions[0], (newAction(schema.GroupVersionKind{"test.cvo.io", "v1", "TestA"}, "default", "testa")); !reflect.DeepEqual(got, exp) {
				t.Fatalf("expected: %s got: %s", spew.Sdump(exp), spew.Sdump(got))
			}
			if got, exp := actions[1], (newAction(schema.GroupVersionKind{"test.cvo.io", "v1", "TestB"}, "default", "testb")); !reflect.DeepEqual(got, exp) {
				t.Fatalf("expected: %s got: %s", spew.Sdump(exp), spew.Sdump(got))
			}
		},
	}, {
		manifests: []string{
			`{
				"apiVersion": "test.cvo.io/v1",
				"kind": "TestA",
				"metadata": {
					"namespace": "default",
					"name": "testa"
				}
			}`,
			`{
				"apiVersion": "test.cvo.io/v1",
				"kind": "TestB",
				"metadata": {
					"namespace": "default",
					"name": "testb"
				}
			}`,
		},
		reactors: map[action]error{
			newAction(schema.GroupVersionKind{"test.cvo.io", "v1", "TestA"}, "default", "testa"): &meta.NoResourceMatchError{},
		},
		wantErr: true,
		check: func(t *testing.T, actions []action) {
			if len(actions) != 3 {
				spew.Dump(actions)
				t.Fatalf("unexpected %d actions", len(actions))
			}

			if got, exp := actions[0], (newAction(schema.GroupVersionKind{"test.cvo.io", "v1", "TestA"}, "default", "testa")); !reflect.DeepEqual(got, exp) {
				t.Fatalf("expected: %s got: %s", spew.Sdump(exp), spew.Sdump(got))
			}
		},
	}, {
		manifests: []string{
			`{
				"apiVersion": "test.cvo.io/v1",
				"kind": "TestA",
				"metadata": {
					"namespace": "default",
					"name": "testa",
					"annotations": {
						"v1.cluster-version-operator.operators.openshift.io/requeue-on-error": "NoMatch"
					}
				}
			}`,
			`{
				"apiVersion": "test.cvo.io/v1",
				"kind": "TestB",
				"metadata": {
					"namespace": "default",
					"name": "testb"
				}
			}`,
		},
		reactors: map[action]error{
			newAction(schema.GroupVersionKind{"test.cvo.io", "v1", "TestA"}, "default", "testa"): &meta.NoResourceMatchError{},
		},
		wantErr: true,
		check: func(t *testing.T, actions []action) {
			if len(actions) != 7 {
				spew.Dump(actions)
				t.Fatalf("unexpected %d actions", len(actions))
			}

			if got, exp := actions[0], (newAction(schema.GroupVersionKind{"test.cvo.io", "v1", "TestA"}, "default", "testa")); !reflect.DeepEqual(got, exp) {
				t.Fatalf("expected: %s got: %s", spew.Sdump(exp), spew.Sdump(got))
			}
			if got, exp := actions[3], (newAction(schema.GroupVersionKind{"test.cvo.io", "v1", "TestB"}, "default", "testb")); !reflect.DeepEqual(got, exp) {
				t.Fatalf("expected: %s got: %s", spew.Sdump(exp), spew.Sdump(got))
			}
			if got, exp := actions[4], (newAction(schema.GroupVersionKind{"test.cvo.io", "v1", "TestA"}, "default", "testa")); !reflect.DeepEqual(got, exp) {
				t.Fatalf("expected: %s got: %s", spew.Sdump(exp), spew.Sdump(got))
			}
		},
	}, {
		manifests: []string{
			`{
				"apiVersion": "test.cvo.io/v1",
				"kind": "TestA",
				"metadata": {
					"namespace": "default",
					"name": "testa",
					"annotations": {
						"v1.cluster-version-operator.operators.openshift.io/requeue-on-error": "NoMatch"
					}
				}
			}`,
			`{
				"apiVersion": "test.cvo.io/v1",
				"kind": "TestB",
				"metadata": {
					"namespace": "default",
					"name": "testb",
					"annotations": {
						"v1.cluster-version-operator.operators.openshift.io/requeue-on-error": "NoMatch"
					}
				}
			}`,
		},
		reactors: map[action]error{
			newAction(schema.GroupVersionKind{"test.cvo.io", "v1", "TestA"}, "default", "testa"): &meta.NoResourceMatchError{},
			newAction(schema.GroupVersionKind{"test.cvo.io", "v1", "TestB"}, "default", "testb"): &meta.NoResourceMatchError{},
		},
		wantErr: true,
		check: func(t *testing.T, actions []action) {
			if len(actions) != 9 {
				spew.Dump(actions)
				t.Fatalf("unexpected %d actions", len(actions))
			}

			if got, exp := actions[0], (newAction(schema.GroupVersionKind{"test.cvo.io", "v1", "TestA"}, "default", "testa")); !reflect.DeepEqual(got, exp) {
				t.Fatalf("expected: %s got: %s", spew.Sdump(exp), spew.Sdump(got))
			}
			if got, exp := actions[3], (newAction(schema.GroupVersionKind{"test.cvo.io", "v1", "TestB"}, "default", "testb")); !reflect.DeepEqual(got, exp) {
				t.Fatalf("expected: %s got: %s", spew.Sdump(exp), spew.Sdump(got))
			}
			if got, exp := actions[6], (newAction(schema.GroupVersionKind{"test.cvo.io", "v1", "TestA"}, "default", "testa")); !reflect.DeepEqual(got, exp) {
				t.Fatalf("expected: %s got: %s", spew.Sdump(exp), spew.Sdump(got))
			}
		},
	}}
	for idx, test := range tests {
		t.Run(fmt.Sprintf("test#%d", idx), func(t *testing.T) {
			var manifests []lib.Manifest
			for _, s := range test.manifests {
				m := lib.Manifest{}
				if err := json.Unmarshal([]byte(s), &m); err != nil {
					t.Fatal(err)
				}
				manifests = append(manifests, m)
			}

			up := &updatePayload{ReleaseImage: "test", ReleaseVersion: "v0.0.0", Manifests: manifests}
			r := &recorder{}
			testMapper := resourcebuilder.NewResourceMapper()
			testMapper.RegisterGVK(schema.GroupVersionKind{"test.cvo.io", "v1", "TestA"}, newTestBuilder(r, test.reactors))
			testMapper.RegisterGVK(schema.GroupVersionKind{"test.cvo.io", "v1", "TestB"}, newTestBuilder(r, test.reactors))
			testMapper.AddToMap(resourcebuilder.Mapper)

			worker := &SyncWorker{}
			worker.backoff.Steps = 3
			worker.builder = NewResourceBuilder(nil)
			ctx := context.Background()
			worker.apply(ctx, up, &SyncWork{}, &statusWrapper{w: worker, previousStatus: worker.Status()})
			test.check(t, r.actions)
		})
	}
}

func Test_SyncWorker_apply_generic(t *testing.T) {
	tests := []struct {
		manifests []string
		modifiers []resourcebuilder.MetaV1ObjectModifierFunc

		check func(t *testing.T, client *dynamicfake.FakeDynamicClient)
	}{
		{
			manifests: []string{
				`{
				"apiVersion": "test.cvo.io/v1",
				"kind": "TestA",
				"metadata": {
					"namespace": "default",
					"name": "testa"
				}
			}`,
				`{
				"apiVersion": "test.cvo.io/v1",
				"kind": "TestB",
				"metadata": {
					"namespace": "default",
					"name": "testb"
				}
			}`,
			},
			check: func(t *testing.T, client *dynamicfake.FakeDynamicClient) {
				actions := client.Actions()
				if len(actions) != 4 {
					spew.Dump(actions)
					t.Fatal("expected only 4 actions")
				}

				got := actions[1].(clientgotesting.CreateAction).GetObject()
				exp := &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "test.cvo.io/v1",
						"kind":       "TestA",
						"metadata": map[string]interface{}{
							"name":      "testa",
							"namespace": "default",
						},
					},
				}
				if !reflect.DeepEqual(got, exp) {
					t.Fatalf("expected: %s got: %s", spew.Sdump(exp), spew.Sdump(got))
				}

				got = actions[3].(clientgotesting.CreateAction).GetObject()
				exp = &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "test.cvo.io/v1",
						"kind":       "TestB",
						"metadata": map[string]interface{}{
							"name":      "testb",
							"namespace": "default",
						},
					},
				}
				if !reflect.DeepEqual(got, exp) {
					t.Fatalf("expected: %s got: %s", spew.Sdump(exp), spew.Sdump(got))
				}
			},
		},
		{
			modifiers: []resourcebuilder.MetaV1ObjectModifierFunc{
				func(obj metav1.Object) {
					m := obj.GetLabels()
					if m == nil {
						m = make(map[string]string)
					}
					m["test/label"] = "a"
					obj.SetLabels(m)
				},
			},
			manifests: []string{
				`{
					"apiVersion": "test.cvo.io/v1",
					"kind": "TestA",
					"metadata": {
						"namespace": "default",
						"name": "testa"
					}
				}`,
				`{
					"apiVersion": "test.cvo.io/v1",
					"kind": "TestB",
					"metadata": {
						"namespace": "default",
						"name": "testb"
					}
				}`,
			},
			check: func(t *testing.T, client *dynamicfake.FakeDynamicClient) {
				actions := client.Actions()
				if len(actions) != 4 {
					spew.Dump(actions)
					t.Fatalf("got %d actions", len(actions))
				}

				got := actions[1].(clientgotesting.CreateAction).GetObject()
				exp := &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "test.cvo.io/v1",
						"kind":       "TestA",
						"metadata": map[string]interface{}{
							"name":      "testa",
							"namespace": "default",
							"labels":    map[string]interface{}{"test/label": "a"},
						},
					},
				}
				if !reflect.DeepEqual(got, exp) {
					t.Fatalf("expected: %s got: %s", spew.Sdump(exp), spew.Sdump(got))
				}

				got = actions[3].(clientgotesting.CreateAction).GetObject()
				exp = &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "test.cvo.io/v1",
						"kind":       "TestB",
						"metadata": map[string]interface{}{
							"name":      "testb",
							"namespace": "default",
							"labels":    map[string]interface{}{"test/label": "a"},
						},
					},
				}
				if !reflect.DeepEqual(got, exp) {
					t.Fatalf("expected: %s got: %s", spew.Sdump(exp), spew.Sdump(got))
				}
			},
		},
	}
	for idx, test := range tests {
		t.Run(fmt.Sprintf("test#%d", idx), func(t *testing.T) {
			var manifests []lib.Manifest
			for _, s := range test.manifests {
				m := lib.Manifest{}
				if err := json.Unmarshal([]byte(s), &m); err != nil {
					t.Fatal(err)
				}
				manifests = append(manifests, m)
			}

			dynamicScheme := runtime.NewScheme()
			dynamicScheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "test.cvo.io", Version: "v1", Kind: "TestA"}, &unstructured.Unstructured{})
			dynamicScheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "test.cvo.io", Version: "v1", Kind: "TestB"}, &unstructured.Unstructured{})
			dynamicClient := dynamicfake.NewSimpleDynamicClient(dynamicScheme)

			up := &updatePayload{ReleaseImage: "test", ReleaseVersion: "v0.0.0", Manifests: manifests}
			worker := &SyncWorker{}
			worker.backoff.Steps = 1
			worker.builder = &testResourceBuilder{
				client:    dynamicClient,
				modifiers: test.modifiers,
			}
			ctx := context.Background()
			err := worker.apply(ctx, up, &SyncWork{}, &statusWrapper{w: worker, previousStatus: worker.Status()})
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, dynamicClient)
		})
	}
}

type testBuilder struct {
	*recorder
	reactors  map[action]error
	modifiers []resourcebuilder.MetaV1ObjectModifierFunc

	m *lib.Manifest
}

func (t *testBuilder) WithModifier(m resourcebuilder.MetaV1ObjectModifierFunc) resourcebuilder.Interface {
	t.modifiers = append(t.modifiers, m)
	return t
}

func (t *testBuilder) Do() error {
	a := t.recorder.Invoke(t.m.GVK, t.m.Object().GetNamespace(), t.m.Object().GetName())
	return t.reactors[a]
}

func newTestBuilder(r *recorder, rts map[action]error) resourcebuilder.NewInteraceFunc {
	return func(_ *rest.Config, m lib.Manifest) resourcebuilder.Interface {
		return &testBuilder{recorder: r, reactors: rts, m: &m}
	}
}

type recorder struct {
	actions []action
}

func (r *recorder) Invoke(gvk schema.GroupVersionKind, namespace, name string) action {
	action := action{GVK: gvk, Namespace: namespace, Name: name}
	r.actions = append(r.actions, action)
	return action
}

type action struct {
	GVK       schema.GroupVersionKind
	Namespace string
	Name      string
}

func newAction(gvk schema.GroupVersionKind, namespace, name string) action {
	return action{GVK: gvk, Namespace: namespace, Name: name}
}

type fakeSyncRecorder struct {
	Returns *SyncWorkerStatus
	Updates []configv1.Update
}

func (r *fakeSyncRecorder) StatusCh() <-chan SyncWorkerStatus {
	ch := make(chan SyncWorkerStatus)
	close(ch)
	return ch
}

func (r *fakeSyncRecorder) Start(stopCh <-chan struct{}) {}

func (r *fakeSyncRecorder) Update(desired configv1.Update, overrides []configv1.ComponentOverride, reconciling bool) *SyncWorkerStatus {
	r.Updates = append(r.Updates, desired)
	return r.Returns
}

type fakeResourceBuilder struct {
	M   []*lib.Manifest
	Err error
}

func (b *fakeResourceBuilder) Apply(m *lib.Manifest) error {
	b.M = append(b.M, m)
	return b.Err
}

type fakeDirectoryRetriever struct {
	Path string
	Err  error
}

func (r *fakeDirectoryRetriever) RetrievePayload(ctx context.Context, update configv1.Update) (string, error) {
	return r.Path, r.Err
}

type fakePayloadRetriever struct {
	Dir string
	Err error
}

func (r *fakePayloadRetriever) RetrievePayload(ctx context.Context, desired configv1.Update) (string, error) {
	return r.Dir, r.Err
}

// testResourceBuilder uses a fake dynamic client to exercise the generic builder in tests.
type testResourceBuilder struct {
	client    *dynamicfake.FakeDynamicClient
	modifiers []resourcebuilder.MetaV1ObjectModifierFunc
}

func (b *testResourceBuilder) Apply(m *lib.Manifest) error {
	ns := m.Object().GetNamespace()
	fakeGVR := schema.GroupVersionResource{Group: m.GVK.Group, Version: m.GVK.Version, Resource: strings.ToLower(m.GVK.Kind)}
	client := b.client.Resource(fakeGVR).Namespace(ns)
	builder, err := internal.NewGenericBuilder(client, *m)
	if err != nil {
		return err
	}
	for _, m := range b.modifiers {
		builder = builder.WithModifier(m)
	}
	return builder.Do()
}
