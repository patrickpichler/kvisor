package version

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestVersion(t *testing.T) {
	r := require.New(t)
	v := version.Info{
		Major:     "1",
		Minor:     "21+",
		GitCommit: "2812f9fb0003709fc44fc34166701b377020f1c9",
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Errorf("unexpected encoding error: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write(b)
		r.NoError(err)
	}))
	defer s.Close()
	client := kubernetes.NewForConfigOrDie(&rest.Config{Host: s.URL})

	got, err := Get(client)
	if err != nil {
		return
	}

	r.NoError(err)
	r.Equal("1.21+", got.Full)
	r.Equal(21, got.MinorInt)
}
