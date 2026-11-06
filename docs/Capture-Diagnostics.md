# Capture diagnostics from a running provider

Capture a live provider before restarting it to preserve evidence of high memory or CPU use. For initial setup, see [Installation](/full-bars/sn/wiki/Installation).

## Capture a bundle

1. Sign in as the user that runs the provider. On a multi-provider host, use the intended provider's own account without sudo.
2. Check that the provider is still running with `urnet-tools status`.
3. Run:

```sh
urnet-tools diag
```

4. Wait for the command to print the bundle directory. The provider keeps serving during the capture. The CPU profile normally takes 30 seconds.
5. Read `summary.txt` in that directory first. It lists the captured files and any incomplete or omitted CPU profile.

The bundle is under the provider's state directory in `diag/`. It includes a heap profile and goroutine stacks. The CPU profile can be omitted if capture or finalization fails. A capture needs at least 512 MiB free on that filesystem.

The profiles can contain session tokens. Treat the entire bundle as a secret and do not post it publicly.

## If capture fails

If the provider has stopped, start it and wait for the problem to recur before capturing. Restarting first loses the live evidence. If the control socket is unavailable, check status and retry after startup. If space is low, free space on the filesystem that holds the state directory. If another capture is running, wait for it to finish.
