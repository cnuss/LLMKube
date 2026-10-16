package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The listing must come from the same place the init container downloads from,
// or a glob expands against one repository and fetches from another. A repo
// source (hf://, a bare repo ID, a huggingface.co repo URL) stages from
// HF_ENDPOINT when the Secret names one and from huggingface.co otherwise; a
// repo base on the HF_ENDPOINT origin stages from the mirror. Each lists from
// that host's API root (for a mirror, the endpoint URL, path included).
// Anything else has no listing.
func TestHFListingTreeURL(t *testing.T) {
	const ep = "https://artifactory.corp.example/artifactory/api/huggingfaceml/hf-remote"
	tests := []struct {
		name     string
		source   string
		endpoint string
		want     string
	}{
		{"hf scheme", "hf://Qwen/Qwen2.5-0.5B-Instruct", "", "https://huggingface.co/api/models/Qwen/Qwen2.5-0.5B-Instruct/tree/main?recursive=true"},
		{"hf scheme with revision", "hf://Qwen/Qwen2.5-0.5B-Instruct@7ae557604adf67be50417f59c2c2f167def9a775", "", "https://huggingface.co/api/models/Qwen/Qwen2.5-0.5B-Instruct/tree/7ae557604adf67be50417f59c2c2f167def9a775?recursive=true"},
		{"hf scheme lists from the mirror", "hf://Qwen/Qwen2.5-0.5B-Instruct", ep, ep + "/api/models/Qwen/Qwen2.5-0.5B-Instruct/tree/main?recursive=true"},
		{"huggingface.co resolve base", "https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct/resolve/main/", "", "https://huggingface.co/api/models/Qwen/Qwen2.5-0.5B-Instruct/tree/main?recursive=true"},
		{"huggingface.co resolve base lists from the mirror", "https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct/resolve/main/", ep, ep + "/api/models/Qwen/Qwen2.5-0.5B-Instruct/tree/main?recursive=true"},
		{"bare repo id", "nvidia/GLM-5.3-Flash-NVFP4", "", "https://huggingface.co/api/models/nvidia/GLM-5.3-Flash-NVFP4/tree/main?recursive=true"},
		{"bare repo id lists from the mirror", "nvidia/GLM-5.3-Flash-NVFP4", ep, ep + "/api/models/nvidia/GLM-5.3-Flash-NVFP4/tree/main?recursive=true"},
		{"mirror repo base", ep + "/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/", ep, ep + "/api/models/nvidia/GLM-5.3-Flash-NVFP4/tree/main?recursive=true"},
		{"mirror repo base without trailing slash", ep + "/nvidia/GLM-5.3-Flash-NVFP4/resolve/main", ep + "/", ep + "/api/models/nvidia/GLM-5.3-Flash-NVFP4/tree/main?recursive=true"},
		{"mirror revision is path-escaped", ep + "/org/repo/resolve/refs%2Fpr%2F1/", ep, ep + "/api/models/org/repo/tree/refs%2Fpr%2F1?recursive=true"},
		{"mirror origin but outside the endpoint path", "https://artifactory.corp.example/other/org/repo/resolve/main/", ep, ""},
		{"mirror base without HF_ENDPOINT", ep + "/org/repo/resolve/main/", "", ""},
		{"lookalike host", "https://artifactory.corp.example.evil.example/artifactory/api/huggingfaceml/hf-remote/org/repo/resolve/main/", ep, ""},
		{"mirror path that is not a resolve base", ep + "/org/repo/blob/main/", ep, ""},
		{"s3", "s3://bucket/prefix", ep, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := hfListingTreeURL(tt.source, tt.endpoint)
			if ok != (tt.want != "") || got != tt.want {
				t.Errorf("hfListingTreeURL(%q, %q) = %q, %v; want %q", tt.source, tt.endpoint, got, ok, tt.want)
			}
		})
	}
}

// Fed the bytes each producer actually returns for the same request, recorded
// from a JFrog Artifactory huggingfaceml remote and from huggingface.co. The
// listing keeps files and drops directory entries.
func TestListHFRepoFiles_RealResponses(t *testing.T) {
	tests := []struct {
		fixture string
		want    []string
	}{
		{"artifactory-tree-qwen.json", qwenRepoFiles},
		{"huggingface-tree-qwen.json", qwenRepoFiles},
		{"artifactory-tree-glm.json", glmRepoFiles()},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			body, err := os.ReadFile("testdata/hflisting/" + tt.fixture)
			if err != nil {
				t.Fatal(err)
			}
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer srv.Close()

			got, err := listHFRepoFiles(context.Background(), loopbackSafeClient(), srv.URL+"/api/models/org/repo/tree/main?recursive=true", "hf_tok")
			if err != nil {
				t.Fatalf("listHFRepoFiles: %v", err)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("files =\n%v\nwant\n%v", got, tt.want)
			}
			if gotAuth != "Bearer hf_tok" {
				t.Errorf("Authorization = %q, want the bearer token", gotAuth)
			}
		})
	}
}

// huggingface.co pages a large tree with an RFC 8288 Link header; stopping at
// the first page would silently drop shards from a glob.
func TestListHFRepoFiles_FollowsLinkPagination(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/models/org/repo/tree/main?recursive=true&cursor=p2>; rel="next"`, srv.URL))
			_, _ = w.Write([]byte(`[{"type":"file","path":"a.safetensors"},{"type":"directory","path":"sub"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"type":"file","path":"sub/b.safetensors"}]`))
	}))
	defer srv.Close()

	got, err := listHFRepoFiles(context.Background(), loopbackSafeClient(), srv.URL+"/api/models/org/repo/tree/main?recursive=true", "")
	if err != nil {
		t.Fatalf("listHFRepoFiles: %v", err)
	}
	if want := []string{"a.safetensors", "sub/b.safetensors"}; !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
}

// A page link that leaves the listing's origin would carry the token to a host
// the Secret never named, so it is refused rather than followed.
func TestListHFRepoFiles_RefusesCrossOriginPage(t *testing.T) {
	var otherAuth string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[{"type":"file","path":"b.safetensors"}]`))
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/api/models/org/repo/tree/main?cursor=p2>; rel="next"`, other.URL))
		_, _ = w.Write([]byte(`[{"type":"file","path":"a.safetensors"}]`))
	}))
	defer srv.Close()

	_, err := listHFRepoFiles(context.Background(), loopbackSafeClient(), srv.URL+"/api/models/org/repo/tree/main?recursive=true", "hf_tok")
	if err == nil {
		t.Error("listHFRepoFiles followed a page link to another origin")
	}
	if otherAuth != "" {
		t.Errorf("token reached the other origin: %q", otherAuth)
	}
}

func TestListHFRepoFiles_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := listHFRepoFiles(context.Background(), loopbackSafeClient(), srv.URL+"/api/models/org/repo/tree/main?recursive=true", "")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want one naming the 401", err)
	}
}

// A glob in spec.files is expanded against the mirror's listing when the Model
// reconciles, and the expansion is what lands in status.stagedFiles, in spec
// order with files[0] (the primary) first. A listing failure fails the Model
// with the cause rather than marking it Ready with an empty file set.
func TestReconcileRuntimeResolved_ExpandsGlobsFromListing(t *testing.T) {
	body, err := os.ReadFile("testdata/hflisting/artifactory-tree-glm.json")
	if err != nil {
		t.Fatal(err)
	}
	listingStatus := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/models/nvidia/GLM-5.3-Flash-NVFP4/tree/main") {
			http.NotFound(w, r)
			return
		}
		if listingStatus != http.StatusOK {
			http.Error(w, "nope", listingStatus)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	endpoint := srv.URL + "/artifactory/api/huggingfaceml/hf-remote"

	newModel := func() *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "glm", Namespace: "default", Generation: 1},
			Spec: inferencev1alpha1.ModelSpec{
				Source:          endpoint + "/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/",
				Format:          "safetensors",
				SourceSecretRef: &corev1.LocalObjectReference{Name: "huggingface"},
				Files:           []string{"config.json", "*.json", "*.jinja", "*.safetensors"},
			},
		}
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "huggingface", Namespace: "default"},
		Data:       map[string][]byte{"HF_TOKEN": []byte("hf_tok"), "HF_ENDPOINT": []byte(endpoint)},
	}

	t.Run("listing expands the globs", func(t *testing.T) {
		model := newModel()
		r := newMultiFileGateReconciler(t, model, secret)
		r.AllowedRemoteHosts = []string{"127.0.0.1"}
		if _, err := r.reconcileRuntimeResolvedSource(context.Background(), model, computeCacheKey(model.Spec.Source)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		stored := getModel(t, r, "glm")
		if stored.Status.Phase != PhaseReady {
			t.Fatalf("phase = %q, want Ready; conditions=%+v", stored.Status.Phase, stored.Status.Conditions)
		}
		want := []string{"config.json", "generation_config.json", "hf_quant_config.json", "model.safetensors.index.json",
			"processor_config.json", "tokenizer.json", "tokenizer_config.json", "chat_template.jinja"}
		for i := 1; i <= 33; i++ {
			want = append(want, fmt.Sprintf("model-%05d-of-00033.safetensors", i))
		}
		if !reflect.DeepEqual(stored.Status.StagedFiles, want) {
			t.Errorf("stagedFiles =\n%v\nwant\n%v", stored.Status.StagedFiles, want)
		}
	})

	t.Run("listing failure fails the Model", func(t *testing.T) {
		listingStatus = http.StatusUnauthorized
		defer func() { listingStatus = http.StatusOK }()
		model := newModel()
		r := newMultiFileGateReconciler(t, model, secret)
		r.AllowedRemoteHosts = []string{"127.0.0.1"}
		if _, err := r.reconcileRuntimeResolvedSource(context.Background(), model, computeCacheKey(model.Spec.Source)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		stored := getModel(t, r, "glm")
		cond := meta.FindStatusCondition(stored.Status.Conditions, ConditionDegraded)
		if stored.Status.Phase != PhaseFailed || cond == nil || !strings.Contains(cond.Message, "401") {
			t.Errorf("phase = %q, degraded = %+v; want Failed naming the 401", stored.Status.Phase, cond)
		}
		if len(stored.Status.StagedFiles) != 0 {
			t.Errorf("stagedFiles = %v, want none after a failed listing", stored.Status.StagedFiles)
		}
	})
}

// The serving pod's downloader stages the controller's expansion. Without it a
// glob reaches the init container as a literal file name and 404s; with a
// status written for an older spec it would stage the wrong set.
func TestModelStagingPlan_UsesExpandedGlobs(t *testing.T) {
	expanded := []string{"config.json", "a.safetensors", "b.safetensors"}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default", Generation: 2},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://org/repo",
			Format: "safetensors",
			Files:  []string{"config.json", "*.safetensors"},
		},
		Status: inferencev1alpha1.ModelStatus{
			Phase:       PhaseReady,
			StagedFiles: expanded,
			Conditions:  []metav1.Condition{{Type: ConditionAvailable, Status: metav1.ConditionTrue, ObservedGeneration: 2, Reason: "x"}},
		},
	}

	plan, err := modelStagingPlan(model)
	if err != nil {
		t.Fatalf("modelStagingPlan: %v", err)
	}
	if plan.Primary != "config.json" || !reflect.DeepEqual(plan.Files, expanded) {
		t.Errorf("plan = %+v, want primary config.json and files %v", plan, expanded)
	}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "m", Runtime: "vllm"},
	}
	cfg := buildModelStorageConfig(model, isvc, "default", true, "", "", defaultPrefetchImage, 0, nil, "")
	var files string
	for _, c := range cfg.initContainers {
		for _, e := range c.Env {
			if e.Name == "MODEL_FILES" {
				files = e.Value
			}
		}
	}
	if files != strings.Join(expanded, "\n") {
		t.Errorf("MODEL_FILES = %q, want the expanded list", files)
	}

	stale := model.DeepCopy()
	stale.Status.Conditions[0].ObservedGeneration = 1
	if _, err := modelStagingPlan(stale); err == nil {
		t.Error("modelStagingPlan used stagedFiles written for an older generation")
	}
}

var qwenRepoFiles = []string{".gitattributes", "LICENSE", "README.md", "config.json", "generation_config.json",
	"merges.txt", "model.safetensors", "tokenizer.json", "tokenizer_config.json", "vocab.json"}

func glmRepoFiles() []string {
	files := []string{".gitattributes", ".quant_summary.txt", "README.md", "chat_template.jinja", "config.json",
		"generation_config.json", "hf_quant_config.json", "model.safetensors.index.json", "processor_config.json",
		"tokenizer.json", "tokenizer_config.json"}
	for i := 1; i <= 33; i++ {
		files = append(files, fmt.Sprintf("model-%05d-of-00033.safetensors", i))
	}
	sort.Strings(files)
	return files
}

func loopbackSafeClient() *http.Client {
	r := &ModelReconciler{AllowedRemoteHosts: []string{"127.0.0.1"}}
	return r.metadataClient()
}

func getModel(t *testing.T, r *ModelReconciler, name string) *inferencev1alpha1.Model {
	t.Helper()
	m := &inferencev1alpha1.Model{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, m); err != nil {
		t.Fatalf("get Model: %v", err)
	}
	return m
}

// A server that keeps answering with a next link must not hold the reconcile
// forever; the listing gives up at the page cap with an error.
func TestListHFRepoFiles_StopsAtPageCap(t *testing.T) {
	var pages int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Header().Set("Link", fmt.Sprintf(`<%s/api/models/org/repo/tree/main?cursor=%d>; rel="next"`, srv.URL, pages))
		_, _ = w.Write([]byte(`[{"type":"file","path":"a.safetensors"}]`))
	}))
	defer srv.Close()

	_, err := listHFRepoFiles(context.Background(), loopbackSafeClient(), srv.URL+"/api/models/org/repo/tree/main?recursive=true", "")
	if err == nil || !strings.Contains(err.Error(), "pages") {
		t.Fatalf("err = %v, want the page cap", err)
	}
	if pages != maxHFListingPages {
		t.Errorf("fetched %d pages, want exactly %d", pages, maxHFListingPages)
	}
}

// A proxy error page served as 200 must fail the listing, not expand every
// glob to nothing.
func TestListHFRepoFiles_RejectsNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>Bad gateway</html>`))
	}))
	defer srv.Close()

	if _, err := listHFRepoFiles(context.Background(), loopbackSafeClient(), srv.URL+"/api/models/org/repo/tree/main?recursive=true", ""); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v, want a decode error", err)
	}
}

// The controller lists through its SSRF-guarded client, so a listing host
// outside allowedRemoteHosts that resolves to a blocked address is refused and
// fails the listing, and no request reaches it.
func TestRepoFileListing_SSRFGuardRefusesBlockedHost(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	endpoint := srv.URL + "/artifactory/api/huggingfaceml/hf-remote"
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          endpoint + "/org/repo/resolve/main/",
			Files:           []string{"config.json", "*.safetensors"},
			SourceSecretRef: &corev1.LocalObjectReference{Name: "huggingface"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "huggingface", Namespace: "default"},
		Data:       map[string][]byte{"HF_ENDPOINT": []byte(endpoint), "HF_TOKEN": []byte("hf_tok")},
	}
	r := newMultiFileGateReconciler(t, model, secret)
	if _, err := r.repoFileListing(context.Background(), model); err == nil {
		t.Fatal("listing reached a loopback host the SSRF guard should refuse")
	}
	if hit {
		t.Error("request reached the blocked host")
	}
}

func TestNextLink(t *testing.T) {
	base, _ := url.Parse("https://mirror.example/api/models/org/repo/tree/main?recursive=true")
	tests := []struct {
		name    string
		headers []string
		want    string
	}{
		{"none", nil, ""},
		{"next", []string{`<https://mirror.example/p2>; rel="next"`}, "https://mirror.example/p2"},
		{"relative target", []string{`</api/models/org/repo/tree/main?cursor=c2>; rel="next"`}, "https://mirror.example/api/models/org/repo/tree/main?cursor=c2"},
		{"unquoted rel", []string{`<https://mirror.example/p2>; rel=next`}, "https://mirror.example/p2"},
		{"next among several links", []string{`<https://mirror.example/p0>; rel="prev", <https://mirror.example/p2>; rel="next"`}, "https://mirror.example/p2"},
		{"only prev", []string{`<https://mirror.example/p0>; rel="prev"`}, ""},
		{"malformed entry skipped", []string{`https://mirror.example/p2; rel="next"`}, ""},
		{"no params", []string{`<https://mirror.example/p2>`}, ""},
		{"comma inside the target", []string{`<https://mirror.example/p2?cursor=a,b>; rel="next"`}, "https://mirror.example/p2?cursor=a,b"},
		{"comma inside a target among several links", []string{`<https://mirror.example/p0?x=1,2>; rel="prev", <https://mirror.example/p2?cursor=a,b>; rel="next"`}, "https://mirror.example/p2?cursor=a,b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nextLink(tt.headers, base)
			if err != nil || got != tt.want {
				t.Errorf("nextLink(%q) = %q, %v; want %q", tt.headers, got, err, tt.want)
			}
		})
	}
}

// Listing happens only when a glob needs it. Without a glob, or for a source
// that has no listing, repoFileListing makes no request and returns nil, so
// explicit files keep working with no controller egress and a glob on an
// unlistable source is left for ResolveFileSet to reject.
func TestRepoFileListing_NoRequestWithoutNeed(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	endpoint := srv.URL + "/artifactory/api/huggingfaceml/hf-remote"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "huggingface", Namespace: "default"},
		Data:       map[string][]byte{"HF_ENDPOINT": []byte(endpoint)},
	}

	tests := []struct {
		name, source string
		files        []string
	}{
		{"explicit files on a listable source", endpoint + "/org/repo/resolve/main/", []string{"config.json", "model.safetensors"}},
		{"glob on a source with no listing", "s3://bucket/prefix", []string{"*.safetensors"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: tt.source, Files: tt.files,
					SourceSecretRef: &corev1.LocalObjectReference{Name: "huggingface"},
				},
			}
			r := newMultiFileGateReconciler(t, model, secret)
			r.AllowedRemoteHosts = []string{"127.0.0.1"}
			got, err := r.repoFileListing(context.Background(), model)
			if err != nil || got != nil {
				t.Errorf("repoFileListing = %v, %v; want nil, nil", got, err)
			}
		})
	}
	if hits != 0 {
		t.Errorf("made %d listing requests, want none", hits)
	}
}

// An ungated repository lists without a token, so a missing Secret, a missing
// ref, or a Secret without HF_TOKEN yields no token rather than an error. The
// value is trimmed, as an env-projected Secret often ends in a newline.
func TestHFTokenFromSecret(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "huggingface", Namespace: "default"},
		Data:       map[string][]byte{"HF_TOKEN": []byte("hf_tok\n")},
	}
	model := func(ref string) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
		if ref != "" {
			m.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: ref}
		}
		return m
	}
	r := newMultiFileGateReconciler(t, model(""), secret)
	tests := []struct{ name, ref, want string }{
		{"no ref", "", ""},
		{"missing Secret", "absent", ""},
		{"token trimmed", "huggingface", "hf_tok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.hfTokenFromSecret(context.Background(), model(tt.ref)); got != tt.want {
				t.Errorf("hfTokenFromSecret = %q, want %q", got, tt.want)
			}
		})
	}
	if got := (&ModelReconciler{}).hfTokenFromSecret(context.Background(), model("huggingface")); got != "" {
		t.Errorf("nil client: hfTokenFromSecret = %q, want empty", got)
	}
}

// The page cap bounds requests, not time: a server that answers every page
// slowly would otherwise hold the reconcile for up to 256 request timeouts.
// The listing is bounded as a whole.
func TestListHFRepoFiles_BoundedInTime(t *testing.T) {
	defer func(d time.Duration) { hfListingTimeout = d }(hfListingTimeout)
	hfListingTimeout = 200 * time.Millisecond

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		w.Header().Set("Link", fmt.Sprintf(`<%s/api/models/org/repo/tree/main?cursor=%s>; rel="next"`, srv.URL, r.URL.Query().Get("cursor")+"x"))
		_, _ = w.Write([]byte(`[{"type":"file","path":"a.safetensors"}]`))
	}))
	defer srv.Close()

	start := time.Now()
	_, err := listHFRepoFiles(context.Background(), loopbackSafeClient(), srv.URL+"/api/models/org/repo/tree/main?recursive=true", "")
	if err == nil {
		t.Fatal("listing finished despite never ending")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("listing ran %v, want it cut off near %v", elapsed, hfListingTimeout)
	}
}
