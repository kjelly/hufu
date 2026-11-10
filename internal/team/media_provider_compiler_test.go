package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"reflect"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

func TestNativeProviderCompilerKeepsCanonicalReferencesAndOwnsBuffers(t *testing.T) {
	for _, object := range []bool{false, true} {
		name := "call"
		if object {
			name = "object"
		}
		t.Run(name, func(t *testing.T) {
			c, ctx, item, request := nativeMediaProviderFixture(t)
			if object {
				request.ObjectCall = &fantasy.ObjectCall{Prompt: request.Call.Prompt}
				request.Call = nil
			}
			originalMessages, err := providerMediaMessages(request)
			if err != nil {
				t.Fatal(err)
			}
			before, err := agent.EncodeMediaHistory(originalMessages, "compiler-fixture")
			if err != nil {
				t.Fatal(err)
			}
			wire, release, err := (providerRequestAdmission{c: c}).CompileProviderRequest(ctx, request)
			if err != nil || release == nil {
				t.Fatal("valid native compiler did not produce owned buffers", err)
			}
			t.Cleanup(release)
			messages, err := providerMediaMessages(wire)
			if err != nil {
				t.Fatal(err)
			}
			var buffers [][]byte
			for index, ref := range item.MediaBinding.Refs {
				file, ok := messages[0].Content[index+1].(fantasy.FilePart)
				if !ok || file.MediaType != ref.MediaType || file.Filename != "" {
					t.Fatal("compiler dropped a repeated image occurrence or changed its MIME")
				}
				digest := sha256.Sum256(file.Data)
				if hex.EncodeToString(digest[:]) != ref.SHA256 {
					t.Fatal("compiler wire buffer differs from canonical source content")
				}
				buffers = append(buffers, file.Data)
			}
			after, err := agent.EncodeMediaHistory(originalMessages, "compiler-fixture")
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("native compilation overwrote canonical caller history")
			}
			release()
			release()
			for _, data := range buffers {
				if !bytes.Equal(data, make([]byte, len(data))) {
					t.Fatal("compiler cleanup retained raw image bytes")
				}
			}
			assertNativeCompilerCreditAvailable(t)
		})
	}
}

func assertNativeCompilerCreditAvailable(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	credit, err := media.ReserveTransport(ctx, media.MaxConcurrentRawBytes)
	if err != nil {
		t.Fatal("native compiler leaked its transport credit", err)
	}
	credit.Release()
}

func TestNativeProviderCompilerRejectsInvalidInputsWithoutLeakingCredit(t *testing.T) {
	for _, variant := range []string{"cancelled", "corrupt CAS", "metadata-only call", "text-only"} {
		t.Run(variant, func(t *testing.T) {
			c, ctx, item, request := nativeMediaProviderFixture(t)
			switch variant {
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "corrupt CAS":
				store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.projectDir)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(store.mediaPath("data", item.MediaBinding.Refs[0].ID), []byte("corrupt fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "metadata-only call":
				request.Call, request.Messages = nil, request.Call.Prompt
			case "text-only":
				request.Call = &fantasy.Call{Prompt: []fantasy.Message{fantasy.NewUserMessage("text input")}}
			}
			wire, release, err := (providerRequestAdmission{c: c}).CompileProviderRequest(ctx, request)
			if release != nil {
				t.Cleanup(release)
			}
			if variant == "text-only" {
				if err != nil || release != nil || !reflect.DeepEqual(wire, request) {
					t.Fatal("native compiler changed the text-only compatibility path")
				}
			} else if err == nil || release != nil || agent.ProviderRequestHasMedia(wire) {
				t.Fatal("invalid native compilation returned owned image buffers")
			}
			assertNativeCompilerCreditAvailable(t)
		})
	}
}
