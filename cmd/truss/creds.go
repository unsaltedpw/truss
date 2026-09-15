// Credential wiring: which 1Password-mirrored item and field each secret
// this binary needs lives under, read through internal/secrets.Dir (the
// unchanged file-mount seam, docs/port-plan.md §4.7). None of this carries
// a value -- only names -- so nothing here can appear in an error string
// that would violate the "no secret in any error" constraint; secrets.Dir's
// own errors already name only a path, an item and a field, never a value.
//
// internal/config.Config deliberately does not carry these (§4.1, "Refuses
// to: ... Carry any credential"), so cmd/truss reads them itself, once,
// through the same Dir every credential in this project already uses.
package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/beeradb/truss/internal/forge"
	"github.com/beeradb/truss/internal/ledger"
	"github.com/beeradb/truss/internal/notify"
	"github.com/beeradb/truss/internal/secrets"
)

// Item and field names, one constant per credential this binary reads.
// Named here and nowhere else, so a rename is a one-place edit.
const (
	itemLedger           = "gcs-ledger"
	fieldLedgerEndpoint  = "endpoint"
	fieldLedgerAccessKey = "access_key_id"
	fieldLedgerSecretKey = "secret_access_key"

	itemGitHubApp             = "github-app"
	fieldGitHubAppID          = "app_id"
	fieldGitHubInstallationID = "installation_id"
	fieldGitHubPrivateKey     = "private_key"

	itemTelegram          = "telegram-alert"
	fieldTelegramBotToken = "bot_token"
	fieldTelegramChatID   = "chat_id"

	itemCFTokenMint   = "cf-token-mint"
	fieldCFCredential = "credential"

	itemCFInfraAdmin = "cf-infra-admin"
	fieldCFPassword  = "password"

	itemGCPApply        = "gcp-apply"
	fieldGCPCredentials = "credentials"

	itemTofuEncryption  = "tofu-encryption"
	fieldTofuPassphrase = "passphrase"

	// ⚠️ A CLASSIC PAT, AND IT EXISTS BECAUSE AN APP CANNOT CREATE A
	// REPOSITORY UNDER A USER ACCOUNT. `POST /user/repos` is
	// server-to-server: false in GitHub's permissions table and accepts only
	// OAuth tokens and classic PATs -- not fine-grained ones, and not an App
	// installation token. So a consumer whose roots CREATE repositories
	// needs this; one that only adopts existing repositories does not, which
	// is why it is read optionally.
	itemGitHubRepoAdmin  = "github-repo-admin"
	fieldGitHubRepoToken = "password"

	// The tailnet policy file, for a consumer that manages it as code. Same
	// optional shape as the repo-admin token: a consumer with no tailnet
	// never mounts it.
	itemTailscale     = "tailscale-api-key"
	fieldTailscaleKey = "password"
	fieldTailscaleNet = "tailnet"

	// ⚠️ A ROOT CREDENTIAL, NOT A MINTED ONE, AND THE TEST THAT DECIDES
	// THAT IS docs/credentials.md's: a credential is a root when no API can
	// mint it. Hetzner Cloud issues project API tokens from the console
	// only -- there is no endpoint that creates one -- so this cannot be
	// rotated by anything truss runs, and it belongs in the consumer's
	// expiry table with a real date beside it like every other root.
	//
	// Optional, the same shape as the two above: a consumer with no Hetzner
	// project mounts nothing, and requiring it would stop every other
	// deployment on upgrade.
	//
	// ⚠️ HCLOUD_TOKEN IS THE NAME THE PROVIDER READS FROM THE ENVIRONMENT,
	// which is why it is exported rather than passed as a TF_VAR_. Same
	// reasoning as the tailscale pair: there is one hcloud provider in a
	// root, so an environment variable cannot silently re-authenticate a
	// second one. Where the github provider has two -- the App and the PAT
	// -- and GITHUB_TOKEN would capture both.
	itemHetzner       = "hetzner-api"
	fieldHetznerToken = "credential"
)

// loadLedgerConfig builds a ledger.Config from the gcs-ledger item and the
// bucket name config.Config already validated.
func loadLedgerConfig(dir secrets.Dir, bucket string) (ledger.Config, error) {
	endpoint, err := dir.Field(itemLedger, fieldLedgerEndpoint)
	if err != nil {
		return ledger.Config{}, err
	}
	accessKey, err := dir.Field(itemLedger, fieldLedgerAccessKey)
	if err != nil {
		return ledger.Config{}, err
	}
	secretKey, err := dir.Field(itemLedger, fieldLedgerSecretKey)
	if err != nil {
		return ledger.Config{}, err
	}
	return ledger.Config{
		Endpoint:        endpoint,
		Bucket:          bucket,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	}, nil
}

// loadForgeConfig builds a forge.Config from the github-app item and the
// repo name config.Config already validated. baseURL overrides the API
// host; empty leaves forge.New's own default (https://api.github.com) in
// place. It is read from $GITHUB_API_BASE_URL -- optional, and not part of
// config.Config's ten required names, the same way the Vault connection
// details aren't: it exists so a test (or, in principle, GitHub Enterprise)
// can point the client somewhere other than the real host, never so a
// credential can be.
func loadForgeConfig(dir secrets.Dir, repo, baseURL string) (forge.Config, error) {
	appIDStr, err := dir.Field(itemGitHubApp, fieldGitHubAppID)
	if err != nil {
		return forge.Config{}, err
	}
	installIDStr, err := dir.Field(itemGitHubApp, fieldGitHubInstallationID)
	if err != nil {
		return forge.Config{}, err
	}
	pem, err := dir.Field(itemGitHubApp, fieldGitHubPrivateKey)
	if err != nil {
		return forge.Config{}, err
	}
	appID, err := strconv.ParseInt(strings.TrimSpace(appIDStr), 10, 64)
	if err != nil {
		return forge.Config{}, fmt.Errorf("refusing to continue: %s/%s is not a number", itemGitHubApp, fieldGitHubAppID)
	}
	installID, err := strconv.ParseInt(strings.TrimSpace(installIDStr), 10, 64)
	if err != nil {
		return forge.Config{}, fmt.Errorf("refusing to continue: %s/%s is not a number", itemGitHubApp, fieldGitHubInstallationID)
	}
	return forge.Config{
		BaseURL:        baseURL,
		Repo:           repo,
		AppID:          appID,
		InstallationID: installID,
		PrivateKeyPEM:  []byte(pem),
	}, nil
}

// loadTelegram builds a notify.Telegram from the telegram-alert item.
// HTTP is set explicitly because notify.Telegram.Send does not default a
// nil client itself (unlike ledger.Store and forge.Client, which do) -- it
// dereferences it directly, so a notify.Telegram built with a nil HTTP
// panics on the first Send. That is not hypothetical: this constructor
// shipped without it, and every alert would have panicked in production, on
// the one path whose job is saying something went wrong.
func loadTelegram(dir secrets.Dir, baseURL string) (notify.Telegram, error) {
	token, err := dir.Field(itemTelegram, fieldTelegramBotToken)
	if err != nil {
		return notify.Telegram{}, err
	}
	chatID, err := dir.Field(itemTelegram, fieldTelegramChatID)
	if err != nil {
		return notify.Telegram{}, err
	}
	// Bounded, like every other client here: http.DefaultClient has no
	// timeout, and a Telegram that accepts a connection and never answers
	// would hang the pass on its very last step -- after everything else
	// succeeded.
	return notify.Telegram{
		BotToken: token,
		ChatID:   chatID,
		BaseURL:  baseURL,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// loadCFMintToken reads the one hand-made Cloudflare token that can mint
// other tokens -- used only by the credentials root, and by the expiry
// sweep's Cloudflare probe (§4.7).
func loadCFMintToken(dir secrets.Dir) (string, error) {
	return dir.Field(itemCFTokenMint, fieldCFCredential)
}

// loadCFInfraAdminToken reads the token every non-credentials root applies
// under. It is minted BY the credentials root, so it may legitimately not
// exist yet on a first pass -- FieldIfPresent, never Field.
//
// ⚠️ BUT THE ITEM ITSELF MUST BE MOUNTED, AND NOT CHECKING THAT MADE A
// MISTYPED NAME INVISIBLE. FieldIfPresent answers ENOENT identically for
// "credentials/ has not minted this yet" and "there is no such item,
// because the name is wrong" -- and the caller carries on for the first,
// which turns drift detection into a permanent no-op that reports success
// forever. These item names were reverse-engineered from an older apply.sh
// and are the one part of this port with no authoritative source, so the
// wrong-name case has to be the loud one. Raised by the 2026-09-08 code
// audit.
func loadCFInfraAdminToken(dir secrets.Dir) (string, bool, error) {
	mounted, err := dir.ItemMounted(itemCFInfraAdmin)
	if err != nil {
		return "", false, err
	}
	if !mounted {
		return "", false, fmt.Errorf(
			"refusing to continue: no %q item in the credential mirror at %s -- credentials/ mints this token into an item of that name, so either the mirror is not syncing or the item name here is wrong",
			itemCFInfraAdmin, dir.Root)
	}
	return dir.FieldIfPresent(itemCFInfraAdmin, fieldCFPassword)
}

// loadGCPCredentials reads the one GCP key every root's google provider and
// GCS backend read.
func loadGCPCredentials(dir secrets.Dir) (string, error) {
	return dir.Field(itemGCPApply, fieldGCPCredentials)
}

// loadTofuPassphrase reads the state-encryption passphrase, used only by
// the credentials root.
func loadTofuPassphrase(dir secrets.Dir) (string, error) {
	return dir.Field(itemTofuEncryption, fieldTofuPassphrase)
}
