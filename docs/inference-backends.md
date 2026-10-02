# Inference backends

Hive can route agents through gateways for OpenAI-compatible models instead of a CLI subscription. The supported backend IDs for gateways are `vllm`, `llm-d`, `litellm`, `watsonx`, and named gateways such as `openrouter`.

## How routing works

The agent starts Claude Code in bare mode. Hive sets `ANTHROPIC_BASE_URL` to point to the local translator. The translator converts calls for Anthropic Messages into OpenAI-compatible requests and forwards them to the gateway. The backend name selects the upstream route. It is not a separate agent binary.

## Configure a gateway

Use the dashboard's **Governor Config → Model Gateways** UI, or YAML. A gateway needs a name, kind, endpoint, optional key reference, and optional default model. Agents can then set `backend:` to either the built-in kind (`vllm`, `llm-d`, `litellm`, `watsonx`) or to a configured gateway name such as `openrouter`.

Each gateway also accepts an optional `key_name`. This is an audit label for the key (such as "Team inference key"). It records which key a gateway uses so operators can identify keys without access to secret values. The dashboard displays it as "Using key: `<name>`", or "(unnamed)". The `key_name` field is optional on every gateway kind (`litellm`, `openrouter`, `watsonx`, `vllm`). If you omit it, existing gateways stay identical in `hive.yaml`.

LiteLLM also has a dedicated config block:

```yaml
governor:
  litellm:
    endpoint: https://litellm.example.com
    api_key_env: HIVE_LITELLM_API_KEY
    api_key_file: /secrets/litellm_api_key
    default_model: gpt-4o
    ca_bundle: /secrets/litellm-ca.pem
    local_proxy: false
```

Endpoint environment overrides used by v2 include:

- `HIVE_VLLM_ENDPOINT` for `vllm`
- `HIVE_LLMD_ENDPOINT` for `llm-d`
- `HIVE_LITELLM_ENDPOINT` for LiteLLM
- `HIVE_LITELLM_API_KEY` or `api_key_file` for LiteLLM bearer auth
- `HIVE_LITELLM_MODELS` as a comma-separated list of fallback models when discovery fails

Never put the key value itself in YAML.

## OpenRouter scan-to-fund gateway

OpenRouter is both a gateway kind (`kind: openrouter`) and a guided sponsor flow. It routes through the OpenAI-compatible API at `https://openrouter.ai/api/v1`. Agents use it like any other gateway after you store a key.

### Operator setup with Model Gateways

1. Open **Governor Config → Model Gateways**.
2. Add a gateway named `openrouter` with kind `openrouter`. The UI preset fills the endpoint as `https://openrouter.ai/api/v1`.
3. Store the key as a secret value in the UI, or point `api_key_env` / `api_key_file` at an existing key. Hive stores only the file path or env-var name in `hive.yaml`.
4. Pick a default model from discovery or enter the model identifier manually. The curated fallback default is `deepseek/deepseek-chat`.
5. Assign an agent with `backend: openrouter`. Leave `model:` empty to use the default or specify an explicit model ID.

The equivalent YAML configuration is:

```yaml
governor:
  gateways:
    - name: openrouter
      kind: openrouter
      endpoint: https://openrouter.ai/api/v1
      api_key_file: /data/secrets/gateway_openrouter_api_key
      default_model: deepseek/deepseek-chat
      key_name: openrouter-prod-key  # optional: audit label shown in the dashboard, not a secret

agents:
  guide:
    backend: openrouter
    model: deepseek/deepseek-chat
```

### Scan-to-fund flow

The dashboard can start an OAuth PKCE flow for OpenRouter from the sponsor card. A sponsor selects a default model, scans the QR code, approves authorization on OpenRouter, and returns to `/openrouter/callback`. Hive exchanges the code for an API key and updates the `openrouter` gateway.

On a spoke dashboard, Hive writes the key to the secret file store on the PVC. The `hive.yaml` file records only `api_key_file`. On a funded hive, the hub queues the gateway secret and delivers it across the TLS heartbeat response. Spokes behind firewalls receive keys without inbound HTTP requests. The spoke confirms arrival in its status report, and the hub then clears the queue. The hub does not store or display the key.

### Credit display and quotas

A spoke with an OpenRouter key proxies the `/api/v1/key` credit endpoint. It displays limits, usage, and balance, and keeps the key private. The hub only shows delivery status for the gateway. After delivery, the spoke reports credit information directly.

## Model discovery

Hive queries `/v1/models` on OpenAI-compatible gateways. LiteLLM discovery includes bearer authorization when operators set a key. If discovery fails, the UI falls back to configured model lists and marks entries as unverified. Do not treat fallback data as proof of endpoint health.

## Guided example: Watsonx via gateway

The watsonx path uses the same gateway machinery: configure the gateway endpoint and credentials, verify `/v1/models`, then assign an agent to that backend/gateway. Use it as the guided flow for any enterprise gateway:

1. Create the gateway in the Model Gateways UI.
2. Store the key in a secret file or env var reference.
3. Click model discovery and choose an entitlement-visible model.
4. Assign a low-risk agent and run one manual kick.
5. Check agent logs for route/model passthrough and upstream HTTP errors.

## Common failures

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `401` or repeated gateway auth errors | Missing/stale API key or wrong LiteLLM virtual key | Rotate the key, update the env/file reference, restart or reload the hive. |
| Model dropdown empty or unverified | `/v1/models` unreachable or blocked | Check endpoint URL, network policy, TLS CA bundle, and gateway logs. |
| Agent starts but every prompt fails | Endpoint does not implement OpenAI chat completions for the selected model | Select a chat-capable model or fix gateway routing. |
| Connection refused / timeout | Service name or port is wrong, or NetworkPolicy blocks it | From the Hive pod, curl the gateway health and `/v1/models` endpoints. |
| Model rejected despite discovery | The agent model differs from the gateway entitlement name | Use the exact model ID returned by `/v1/models`; Hive passes it through verbatim. |

See also [`src/deploy/inference/README.md`](../src/deploy/inference/README.md) for sample deployments of vLLM in the cluster.
