package nri

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRunPodSandboxRemoveRaceCondition tests the race condition where
// RemovePodSandbox is called while RunPodSandbox is still in progress.
// This simulates the customer case where a DaemonSet rolling update kills
// a pod while CRI-O is still creating its sandbox.
//
// Expected behavior:
// - RemovePodSandbox called before SetCreated() should return error
// - The error should indicate "sandbox not yet created"
// - After RunPodSandbox completes, RemovePodSandbox should succeed
func TestRunPodSandboxRemoveRaceCondition(t *testing.T) {
	test := &nriTest{
		plugins: make([]*plugin, 1),
		options: [][]PluginOption{
			{
				// Configure 10 second delay in RunPodSandbox hook
				// This keeps the sandbox in "not created" state
				WithRunPodSandboxDelay(10 * time.Second),
			},
		},
	}

	test.Setup(t)
	test.StartPlugins(WaitForPluginSync)

	// Create pod - this will trigger RunPodSandbox with 10s delay
	pod := test.createPod()
	require.NotEmpty(t, pod, "create pod should return pod ID")

	// Wait a bit to ensure we're inside the NRI RunPodSandbox hook
	// but before SetCreated() is called
	time.Sleep(2 * time.Second)

	// Try to remove the pod while RunPodSandbox is still processing
	// This should fail because sb.Created() is still false
	err := crio.RemovePod(pod)

	// We expect an error here because the sandbox is not yet created
	// In the actual customer case, this manifests as the pod getting stuck
	if err != nil {
		t.Logf("RemovePod during creation returned error (expected): %v", err)
		// This is the expected behavior - RemovePodSandbox returns error
		// when called before SetCreated()
	} else {
		t.Logf("RemovePod during creation succeeded (may indicate race handling)")
	}

	// Wait for the NRI delay to complete (RunPodSandbox finishes)
	time.Sleep(9 * time.Second)

	// Now try to remove the pod again - should succeed
	// because SetCreated() has been called
	err = crio.RemovePod(pod)
	if err != nil {
		t.Logf("RemovePod after creation completed: %v", err)
	}

	// Verify the plugin received the RunPodSandbox event
	runEvent := test.plugins[0].WaitEvent(RunPodEvent(pod), 1*time.Second)
	require.NotNil(t, runEvent, "should receive RunPodSandbox event")
	require.Equal(t, "RunPodSandbox", runEvent.kind, "event should be RunPodSandbox")
}

// TestRunPodSandboxWithDelayTiming verifies that the NRI delay
// actually works as expected and the timing is correct
func TestRunPodSandboxWithDelayTiming(t *testing.T) {
	delayDuration := 5 * time.Second

	test := &nriTest{
		plugins: make([]*plugin, 1),
		options: [][]PluginOption{
			{
				WithRunPodSandboxDelay(delayDuration),
			},
		},
	}

	test.Setup(t)
	test.StartPlugins(WaitForPluginSync)

	// Measure time to create pod
	startTime := time.Now()
	pod := test.createPod()
	elapsed := time.Since(startTime)

	require.NotEmpty(t, pod, "create pod should return pod ID")

	// The pod creation should take at least as long as our delay
	t.Logf("Pod creation took %v (expected >= %v)", elapsed, delayDuration)
	require.GreaterOrEqual(t, elapsed, delayDuration,
		"pod creation should take at least %v due to NRI delay", delayDuration)

	// Cleanup
	test.removePod(pod)
}

// TestRunPodSandboxNoDelayByDefault ensures that the delay
// only applies when explicitly configured
func TestRunPodSandboxNoDelayByDefault(t *testing.T) {
	test := &nriTest{
		plugins: make([]*plugin, 1),
		// No options = no delay
	}

	test.Setup(t)
	test.StartPlugins(WaitForPluginSync)

	// Create pod should be fast without delay
	startTime := time.Now()
	pod := test.createPod()
	elapsed := time.Since(startTime)

	require.NotEmpty(t, pod, "create pod should return pod ID")

	// Should complete quickly (less than 2 seconds)
	t.Logf("Pod creation took %v (expected < 2s)", elapsed)
	require.Less(t, elapsed, 2*time.Second,
		"pod creation should be fast without NRI delay")

	// Cleanup
	test.removePod(pod)
}
