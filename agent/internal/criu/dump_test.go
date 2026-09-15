package criu

import (
	"strings"
	"testing"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

func TestBuildCRIUConfCompressionAcceleration(t *testing.T) {
	conf := buildCRIUConf(&types.CRIUSettings{
		Compress:             true,
		CompressAcceleration: 65537,
	})
	if !strings.Contains(conf, "compress\ncompress-acceleration 65537\n") {
		t.Fatalf("unexpected CRIU config: %q", conf)
	}
}
