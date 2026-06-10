package lib

import (
	"context"
	"errors"
	"fmt"

	rspec "github.com/opencontainers/runtime-spec/specs-go"
	selinux "github.com/opencontainers/selinux/go-selinux"
	"go.podman.io/storage/pkg/idtools"
	types "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/cri-o/cri-o/internal/config/nsmgr"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
)

func (c *ContainerServer) addSandboxPlatform(sb *sandbox.Sandbox) error {
	context, err := selinux.NewContext(sb.ProcessLabel())
	if err != nil {
		return err
	}

	c.state.processLevels[context["level"]]++

	return nil
}

func (c *ContainerServer) removeSandboxPlatform(sb *sandbox.Sandbox) error {
	processLabel := sb.ProcessLabel()

	context, err := selinux.NewContext(processLabel)
	if err != nil {
		return err
	}

	level := context["level"]

	pl, ok := c.state.processLevels[level]
	if ok {
		c.state.processLevels[level] = pl - 1
		if c.state.processLevels[level] == 0 {
			defer delete(c.state.processLevels, level)

			selinux.ReleaseLabel(processLabel)
		}
	}

	return nil
}

func configNsPath(spec *rspec.Spec, nsType rspec.LinuxNamespaceType) (string, error) {
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type != nsType {
			continue
		}

		if ns.Path == "" {
			return "", errors.New("empty networking namespace")
		}

		return ns.Path, nil
	}

	return "", errors.New("missing networking namespace")
}

// recreateUserNamespace recreates the user namespace from userns_options when the namespace path no longer exists.
// This can happen after CRI-O restart when namespace paths in /var/run are cleaned up.
func (c *ContainerServer) recreateUserNamespace(ctx context.Context, sb *sandbox.Sandbox, usernsOpts *types.UserNamespace) error {
	_, span := log.StartSpan(ctx)
	defer span.End()

	log.Infof(ctx, "recreateUserNamespace called for sandbox %s with mode %v (%d), %d UID mappings, %d GID mappings",
		sb.ID(), usernsOpts.GetMode(), int32(usernsOpts.GetMode()), len(usernsOpts.GetUids()), len(usernsOpts.GetGids()))

	// If there are no UID/GID mappings, nothing to recreate
	if len(usernsOpts.GetUids()) == 0 && len(usernsOpts.GetGids()) == 0 {
		log.Infof(ctx, "No UID/GID mappings configured, skipping user namespace recreation")
		return nil
	}

	// Only recreate for POD mode (mode = 0)
	// Other modes: CONTAINER (1), NODE (2), TARGET (3) don't need recreation
	if usernsOpts.GetMode() != types.NamespaceMode_POD {
		log.Infof(ctx, "User namespace mode is %v (%d), not POD - skipping recreation", usernsOpts.GetMode(), int32(usernsOpts.GetMode()))
		return nil
	}

	uids := make([]idtools.IDMap, 0, len(usernsOpts.GetUids()))
	for _, idMap := range usernsOpts.GetUids() {
		uids = append(uids, idtools.IDMap{
			ContainerID: int(idMap.GetContainerId()),
			HostID:      int(idMap.GetHostId()),
			Size:        int(idMap.GetLength()),
		})
	}

	gids := make([]idtools.IDMap, 0, len(usernsOpts.GetGids()))
	for _, idMap := range usernsOpts.GetGids() {
		gids = append(gids, idtools.IDMap{
			ContainerID: int(idMap.GetContainerId()),
			HostID:      int(idMap.GetHostId()),
			Size:        int(idMap.GetLength()),
		})
	}

	idMappings := idtools.NewIDMappingsFromMaps(uids, gids)
	log.Infof(ctx, "Created ID mappings with %d UID mappings and %d GID mappings", len(uids), len(gids))

	// Create the user namespace using the namespace manager
	nsCfg := &nsmgr.PodNamespacesConfig{
		Namespaces: []*nsmgr.PodNamespaceConfig{
			{Type: nsmgr.USERNS},
		},
		IDMappings: idMappings,
	}

	log.Infof(ctx, "Calling NewPodNamespaces to create user namespace")
	namespaces, err := c.config.NamespaceManager().NewPodNamespaces(nsCfg)
	if err != nil {
		return fmt.Errorf("failed to create user namespace: %w", err)
	}
	log.Infof(ctx, "NewPodNamespaces succeeded, created %d namespaces", len(namespaces))

	if len(namespaces) != 1 {
		return fmt.Errorf("expected 1 namespace, got %d", len(namespaces))
	}

	// Join the newly created user namespace
	nsPath := namespaces[0].Path()
	log.Infof(ctx, "Attempting to join user namespace at path: %s", nsPath)
	if err := sb.UserNsJoin(nsPath); err != nil {
		// Clean up on failure
		if removeErr := namespaces[0].Remove(); removeErr != nil {
			log.Errorf(ctx, "Failed to remove user namespace after join failure: %v", removeErr)
		}
		return fmt.Errorf("failed to join recreated user namespace: %w", err)
	}
	log.Infof(ctx, "Successfully joined user namespace at path: %s", nsPath)

	return nil
}
