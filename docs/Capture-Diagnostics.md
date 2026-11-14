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

The bundle is under the provider's state directory in `diag/`. It includes a heap profile and goroutine stacks. The CPU profile can be omitted if capture or finalization fails. A capture needs at least 512 MiB free on the filesystem that holds the `diag/` directory. If `diag/` is a separate mount, that mount is the one checked.

The provider keeps at most five complete bundles. Before each capture, it removes older complete bundles to make room, and it does this before it checks free space. Copy any bundle you need to keep to a private place before you capture again.

The profiles can contain session tokens. Treat the entire bundle as a secret and do not post it publicly.

## If capture fails

If the provider has stopped, start it and wait for the problem to recur before capturing. Restarting first loses the live evidence. If the control socket is unavailable, check status and retry after startup. If space is low, free space on the filesystem that holds the `diag/` directory. If another capture is running, wait for it to finish.

## What you see and what to do when it fails

Run `urnet-tools diag` as the user that runs the provider. The command prints progress and takes about 40 seconds. On success, it prints `Diagnostic bundle:` followed by a path, then the memory use, the goroutine count and the file names. Read `summary.txt` in that directory first.

| Message | What to do |
| :--- | :--- |
| `not allowed to open the control socket` | The socket is owner-only. Run the command as the provider user or with `sudo`. |
| `the control socket ... is not available` | The provider may still be starting. Run `urnet-tools status`, wait a minute and run `diag` again. |
| `did not answer within ...` | A capture may still be running. Look for a new bundle under `<state dir>/diag/`, then try again in a minute. |
| `connection ... closed before it answered` | A hot swap or restart interrupted the capture and left no files. Run `diag` again when `urnet-tools status` shows the provider running. |
| `too old to know diag` | Run `urnet-tools update`, then `urnet-tools hotswap`, then run `diag` again. |
