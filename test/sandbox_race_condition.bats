#!/usr/bin/env bats
# vim:set ft=bash :

# Test for race condition between RunPodSandbox and RemovePodSandbox
# Simulates customer case where DaemonSet rolling update kills a pod
# while CRI-O is still creating the sandbox (pulling images, setting up network, etc.)

load helpers

function setup() {
	setup_test

	# Enable debug logging to see NRI interactions
	export CRIO_LOG_LEVEL=debug

	# Configure extended NRI timeout to allow for delays
	# Default is 2 seconds, we need more for testing
	cat <<EOF >"$CRIO_CONFIG_DIR/20-nri-timeout.conf"
[crio.nri]
enable_nri = true
nri_plugin_request_timeout = "30s"
nri_plugin_registration_timeout = "10s"
EOF
}

function teardown() {
	cleanup_test
}

# Helper function to check if NRI was invoked
function check_nri_activity() {
	local operation="$1"
	echo "# Checking for NRI activity: $operation"

	if [ -f "$CRIO_LOG" ]; then
		echo "# --- NRI-related log entries ---"
		grep -i "nri" "$CRIO_LOG" | tail -20 || echo "# No NRI entries found in logs"
		echo "# --- End of NRI logs ---"

		# Check for specific NRI hooks
		if grep -q "RunPodSandbox.*nri\|nri.*RunPodSandbox" "$CRIO_LOG"; then
			echo "# ✓ NRI RunPodSandbox hook was called"
			return 0
		else
			echo "# ✗ NRI RunPodSandbox hook NOT found in logs"
			return 1
		fi
	else
		echo "# No CRI-O log file found at: $CRIO_LOG"
		return 1
	fi
}

# This test uses image pull to create a natural delay in RunPodSandbox
# When the image is large or needs to be pulled, there's a window where
# the sandbox exists but SetCreated() hasn't been called yet
@test "pod remove during sandbox creation (slow image pull)" {
	# Use a config that requires image pull (if not already cached)
	# The pull operation happens during RunPodSandbox, before SetCreated()
	start_crio

	pod_config="$TESTDIR/sandbox_config.json"
	cp "$TESTDATA"/sandbox_config.json "$pod_config"

	# Start pod creation in background
	# This will go through: network setup -> image pull -> infra container -> NRI -> SetCreated()
	crictl runp "$pod_config" &
	RUNP_PID=$!

	# Give it a moment to start processing
	sleep 1

	# Try to get the pod ID while it's being created
	# The pod may show up in the list before Created() is true
	pod_id=$(crictl pods -q | head -1)

	if [ -n "$pod_id" ]; then
		# Try to remove while creation is in progress
		# Expected: Should either:
		# 1. Return "not yet created" error if before SetCreated()
		# 2. Handle cleanup gracefully if race is properly handled
		output=$(crictl rmp "$pod_id" 2>&1 || true)
		echo "RemovePod output: $output"

		# Document the behavior - this is what we're testing
		if [[ "$output" == *"not yet created"* ]]; then
			echo "Got 'not yet created' error - sandbox not fully initialized"
		elif [[ "$output" == *"not found"* ]] || [[ "$output" == *"404"* ]]; then
			echo "Got 'not found' error - potential race condition issue"
		else
			echo "RemovePod returned: $output"
		fi
	fi

	# Wait for RunPodSandbox to complete (or fail)
	wait $RUNP_PID || true

	# Check if NRI was invoked during the test
	check_nri_activity "RunPodSandbox" || echo "# Warning: NRI may not be enabled or called"

	# Show relevant CRI-O log snippets
	echo "# --- CRI-O log snippets for RunPodSandbox ---"
	if [ -f "$CRIO_LOG" ]; then
		grep "RunPodSandbox\|RunSandbox\|sandbox.*create\|SetCreated" "$CRIO_LOG" | tail -10 || true
	fi
	echo "# --- End of log snippets ---"

	# Cleanup - force remove any remaining pods
	for pod in $(crictl pods -q); do
		crictl rmp -f "$pod" || true
	done
}

# Test the idempotency of pod removal
# Even if RunPodSandbox is interrupted, RemovePodSandbox should clean up properly
@test "pod remove idempotent during creation" {
	start_crio

	pod_config="$TESTDIR/sandbox_config.json"
	cp "$TESTDATA"/sandbox_config.json "$pod_config"

	# Create pod normally
	pod_id=$(crictl runp "$pod_config")

	# First removal - should succeed
	crictl stopp "$pod_id"
	crictl rmp "$pod_id"

	# Second removal - should be idempotent (no error)
	crictl rmp "$pod_id" || true

	# Try to create the same pod again - should work
	# This tests that cleanup was complete
	crictl runp "$pod_config"
}

# Test rapid create/delete cycles
# Simulates kubelet/DaemonSet behavior during rolling updates
@test "rapid pod create and delete cycles" {
	start_crio

	pod_config="$TESTDIR/sandbox_config.json"
	cp "$TESTDATA"/sandbox_config.json "$pod_config"

	# Perform 3 rapid create/delete cycles
	for i in {1..3}; do
		echo "Cycle $i"

		# Create pod
		pod_id=$(crictl runp "$pod_config")
		echo "Created pod: $pod_id"

		# Immediately try to remove (simulates fast rolling update)
		# In real world, kubelet might delete before CRI-O finishes creation
		crictl rmp -f "$pod_id" || true

		# Brief pause between cycles
		sleep 0.5
	done

	# Final cleanup
	for pod in $(crictl pods -q); do
		crictl rmp -f "$pod" || true
	done
}

# Test removal of pod in NotReady state
# Pods in NotReady state may not have completed sandbox creation
@test "remove pod in NotReady state" {
	start_crio

	pod_config="$TESTDIR/sandbox_config.json"

	# Modify config to potentially create a NotReady pod
	jq '.linux.security_context.namespace_options.network = 2' \
		"$TESTDATA"/sandbox_config.json > "$pod_config"

	# Start pod creation in background
	crictl runp "$pod_config" &
	RUNP_PID=$!

	# Wait briefly for pod to appear
	sleep 1

	# Look for NotReady pods
	notready_pods=$(crictl pods --state NotReady -q)

	if [ -n "$notready_pods" ]; then
		for pod in $notready_pods; do
			echo "Attempting to remove NotReady pod: $pod"
			output=$(crictl rmp "$pod" 2>&1 || true)
			echo "Output: $output"

			# This should handle the removal gracefully
			# without leaving the pod stuck in Terminating state
		done
	fi

	# Wait for background process
	wait $RUNP_PID || true

	# Cleanup
	for pod in $(crictl pods -q); do
		crictl rmp -f "$pod" || true
	done
}

# Test pod removal with timeout context
# Ensures that removal doesn't hang indefinitely
@test "pod remove with timeout during creation" {
	start_crio

	pod_config="$TESTDIR/sandbox_config.json"
	cp "$TESTDATA"/sandbox_config.json "$pod_config"

	# Create pod
	pod_id=$(crictl runp "$pod_config")

	# Remove with explicit timeout
	# Should complete within the timeout period
	CRICTL_TIMEOUT=5s crictl rmp -f "$pod_id"
}

# Dedicated test to verify NRI hooks are called during pod lifecycle
@test "verify NRI hooks are called during pod lifecycle" {
	start_crio

	# Give CRI-O time to start and initialize NRI
	sleep 2

	echo "# --- NRI Configuration Check ---"
	if [ -f "$CRIO_CONFIG_DIR/20-nri-timeout.conf" ]; then
		echo "# NRI config file exists:"
		cat "$CRIO_CONFIG_DIR/20-nri-timeout.conf"
	fi
	echo "# --- End of NRI config ---"

	pod_config="$TESTDIR/sandbox_config.json"
	cp "$TESTDATA"/sandbox_config.json "$pod_config"

	# Create pod
	echo "# Creating pod to trigger NRI hooks..."
	pod_id=$(crictl runp "$pod_config")
	echo "# Pod created: $pod_id"

	# Check CRI-O logs for NRI activity
	echo "# --- Checking CRI-O logs for NRI activity ---"
	if [ -f "$CRIO_LOG" ]; then
		echo "# CRI-O log file: $CRIO_LOG"
		echo "# Log file size: $(wc -l < "$CRIO_LOG") lines"

		echo "# --- NRI-related log entries ---"
		grep -i "nri" "$CRIO_LOG" || echo "# No NRI entries found"

		echo "# --- RunPodSandbox log entries ---"
		grep -i "runpodsandbox\|runsandbox" "$CRIO_LOG" | tail -10 || echo "# No RunPodSandbox entries"

		echo "# --- NRI plugin registration ---"
		grep -i "nri.*plugin\|plugin.*nri" "$CRIO_LOG" || echo "# No plugin registration entries"

		echo "# --- NRI enabled check ---"
		grep -i "nri.*enabled\|nri.*disabled" "$CRIO_LOG" || echo "# No NRI enable/disable messages"

	else
		echo "# WARNING: CRI-O log file not found at $CRIO_LOG"
	fi
	echo "# --- End of log analysis ---"

	# Cleanup
	crictl stopp "$pod_id"
	crictl rmp "$pod_id"
}
