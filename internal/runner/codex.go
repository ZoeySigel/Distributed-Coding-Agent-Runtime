package runner

import "strconv"

// CodexArgs is shared by the container runner and the pinned-CLI contract test.
func CodexArgs(baseURL, model string, catalog ...string) []string {
	args := []string{"exec", "--json", "--sandbox", "danger-full-access", "-c", `approval_policy="never"`, "-c", `model_provider="dcar"`, "-c", `model_providers.dcar.name="DCAR"`, "-c", `model_providers.dcar.base_url=` + strconv.Quote(baseURL), "-c", `model_providers.dcar.env_key="DCAR_ATTEMPT_TOKEN"`, "-c", `model_providers.dcar.wire_api="responses"`, "-c", `model_providers.dcar.supports_websockets=false`}
	if len(catalog) > 0 && catalog[0] != "" {
		args = append(args, "-c", "model_catalog_json="+strconv.Quote(catalog[0]))
	}
	return append(args, "--model", model, "-")
}
