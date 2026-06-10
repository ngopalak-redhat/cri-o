//go:build !linux

package lib

import (
	"context"
	"errors"

	types "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/cri-o/cri-o/internal/lib/sandbox"
)

func (c *ContainerServer) addSandboxPlatform(sb *sandbox.Sandbox) error {
	return nil
}

func (c *ContainerServer) removeSandboxPlatform(sb *sandbox.Sandbox) error {
	return nil
}

func (c *ContainerServer) recreateUserNamespace(ctx context.Context, sb *sandbox.Sandbox, usernsOpts *types.UserNamespace) error {
	return errors.New("user namespaces are not supported on this platform")
}
