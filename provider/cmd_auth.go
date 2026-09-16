package provider

// cmd_auth.go — the `provider auth` CLI command handler.
// Ported from main.go lines 1150-1298.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
	"golang.org/x/term"
)

func auth(opts docopt.Opts) {
	home, err := os.UserHomeDir()
	if err != nil {
		shmLogFatal(10, "could not determine home directory: %v", err)
	}
	urNetworkDir := filepath.Join(home, ".urnetwork")
	jwtPath := filepath.Join(urNetworkDir, "jwt")

	if _, err := os.Stat(jwtPath); !errors.Is(err, os.ErrNotExist) {
		// jwt exists
		if force, _ := opts.Bool("-f"); !force {
			fmt.Printf("%s exists. Overwrite? [yN]\n", jwtPath)

			reader := bufio.NewReader(os.Stdin)
			confirm, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
				return
			}

		}
	}

	apiUrl, err := resolveApiUrl(opts)
	if err != nil {
		fmt.Printf("network config error: %s\n", err)
		os.Exit(1)
	}

	maxMemoryHumanReadable, err := opts.String("--max-memory")
	var maxMemory connect.ByteCount
	if err == nil {
		maxMemory, err = connect.ParseByteCount(maxMemoryHumanReadable)
		if err != nil {
			panic(fmt.Errorf("Bad mem argument: %s", maxMemoryHumanReadable))
		}
	}
	if 0 < maxMemory {
		ResizeMessagePoolsPerClass(maxMemory / 8)
		debug.SetMemoryLimit(maxMemory)
	}

	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(event.Ctx())
	defer cancel()

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)

	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)

	var byJwt string
	if userAuth, err := opts.String("--user_auth"); err == nil {
		// user_auth and password

		var password string
		if password, err = opts.String("--password"); err == nil && password == "" {
			fmt.Print("Enter password: ")
			passwordBytes, err := term.ReadPassword(int(syscall.Stdin))
			if err != nil {
				panic(err)
			}
			password = string(passwordBytes)
			fmt.Printf("\n")
		}

		// fmt.Printf("userAuth='%s'; password='%s'\n", userAuth, password)

		loginCallback, loginChannel := connect.NewBlockingApiCallback[*connect.AuthLoginWithPasswordResult](ctx)

		loginArgs := &connect.AuthLoginWithPasswordArgs{
			UserAuth: userAuth,
			Password: password,
		}

		api.AuthLoginWithPassword(loginArgs, loginCallback)

		var loginResult connect.ApiCallbackResult[*connect.AuthLoginWithPasswordResult]
		select {
		case <-ctx.Done():
			tlog("[auth] exiting: signal received during login\n")
			os.Exit(0)
		case loginResult = <-loginChannel:
		}

		if loginResult.Error != nil {
			shmLogFatal(11, "authentication request failed: %v", loginResult.Error)
		}
		if loginResult.Result.Error != nil {
			shmLogFatal(12, "authentication failed: %s", loginResult.Result.Error.Message)
		}
		if loginResult.Result.VerificationRequired != nil {
			shmLogFatal(13, "verification required for %s — complete account setup via the app or web first", loginResult.Result.VerificationRequired.UserAuth)
		}

		byJwt = loginResult.Result.Network.ByJwt
	} else {
		// auth_code
		authCode, _ := opts.String("<auth_code>")
		if authCode == "" {
			fmt.Print("Enter auth code: ")
			authCodeBytes, err := term.ReadPassword(int(syscall.Stdin))
			if err != nil {
				panic(err)
			}
			authCode = strings.TrimSpace(string(authCodeBytes))
			fmt.Printf("\n")
		}

		authCodeLogin := &connect.AuthCodeLoginArgs{
			AuthCode: authCode,
		}

		authCodeLoginCallback, authCodeLoginChannel := connect.NewBlockingApiCallback[*connect.AuthCodeLoginResult](ctx)

		api.AuthCodeLogin(authCodeLogin, authCodeLoginCallback)

		var authCodeLoginResult connect.ApiCallbackResult[*connect.AuthCodeLoginResult]
		select {
		case <-ctx.Done():
			tlog("[auth] exiting: signal received during auth-code login\n")
			os.Exit(0)
		case authCodeLoginResult = <-authCodeLoginChannel:
		}

		if authCodeLoginResult.Error != nil {
			shmLogFatal(14, "authentication code request failed: %v", authCodeLoginResult.Error)
		}
		if authCodeLoginResult.Result.Error != nil {
			shmLogFatal(15, "authentication code rejected: %s — auth codes are single-use; if restarting, mount /root/.urnetwork as a persistent volume", authCodeLoginResult.Result.Error.Message)
		}

		byJwt = authCodeLoginResult.Result.ByJwt
	}

	if byJwt != "" {
		if err := os.MkdirAll(urNetworkDir, 0700); err != nil {
			shmLogFatal(16, "could not create %s: %v", urNetworkDir, err)
		}
		if err := atomicWriteFile(jwtPath, []byte(byJwt), 0600); err != nil {
			shmLogFatal(17, "could not write jwt to %s: %v", jwtPath, err)
		}
		fmt.Printf("Jwt written to %s\n", jwtPath)
	}
}
