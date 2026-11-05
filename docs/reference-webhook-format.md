# Reference Webhook Format

> **This is an internal reference contract, not a real POS provider's specification.**
> It is the format the platform defines for its own tests and demos, and for
> providers that integrate against us directly. Each real POS provider gets
> its own adapter that maps that provider's payload to the same internal model.

- Provider key: `reference`
- Adapter: `internal/ingestion/reference`
- Encoding: a single UTF-8 JSON object

## Example

[`reference-webhook/order_completed.json`](reference-webhook/order_completed.json):

```json
{
  "event_id": "evt_01JC4Z8Q2W",
  "event_type": "order.completed",
  "tenant_ref": "chain-istanbul",
  "order": {
    "restaurant_id": "store-0042",
    "order_id": "ORD-2026-000123",
    "customer_id": "cust-7f3a9c",
    "subtotal_minor": 25000,
    "discount_minor": 2500,
    "tax_minor": 2250,
    "tip_minor": 1500,
    "currency": "TRY",
    "completed_at": "2026-11-05T12:30:00+03:00"
  }
}
```

Eligible amount: `25000 - 2500 = 22500` kuruş (225.00 TRY). Tax and tip are
excluded. With the default rule (1 point per 10 TRY) this earns **22 points**.

A guest order (no `customer_id`) is in
[`reference-webhook/order_completed_guest.json`](reference-webhook/order_completed_guest.json).
It is stored but earns no points.

The sample files are parsed by the adapter's tests, so they always reflect
what the adapter accepts.

## Fields

### Envelope

| Field | Type | Required | Description |
|---|---|---|---|
| `event_id` | string | yes | Unique per delivery within `(provider, tenant_ref)`. Deduplication key. |
| `event_type` | string | yes | Only `order.completed` is processed. Other types are acknowledged and ignored. |
| `tenant_ref` | string | no | Tenant (e.g. restaurant chain) on the provider side. Omitted or `null` → `"default"`. An empty string is invalid. |
| `order` | object | yes | See below. |

### `order`

| Field | Type | Required | Description |
|---|---|---|---|
| `restaurant_id` | string | yes | Provider's store ID, unique within the tenant. |
| `order_id` | string | yes | Provider's order ID, unique within the restaurant. |
| `customer_id` | string \| null | no | Stable provider customer ID, unique within the tenant. Omitted or `null` = guest. An empty string is invalid. |
| `subtotal_minor` | integer | yes | Item subtotal before discounts, in minor units. |
| `discount_minor` | integer | no | Total discount, in minor units. Default `0`. Must not exceed `subtotal_minor`. |
| `tax_minor` | integer | no | Tax, in minor units. Validated, **excluded** from loyalty. |
| `tip_minor` | integer | no | Tip, in minor units. Validated, **excluded** from loyalty. |
| `currency` | string | yes | ISO 4217 code (e.g. `TRY`). Upper-cased by the adapter. |
| `completed_at` | string | yes | RFC 3339 timestamp **with** a time zone offset. Stored as UTC. |

## Rules

- **Money:** all amounts are JSON integers in minor units (kuruş for TRY).
  Strings (`"100"`), fractions (`1.5`) and exponents (`1e3`) are rejected.
  Negative amounts are rejected.
- **Eligible amount:** `subtotal_minor - discount_minor`. Tax and tip never earn points.
- **Identifiers:** non-empty, at most 128 bytes, no leading or trailing whitespace,
  no control characters.
- **Unknown fields** are ignored, so new fields can be added without breaking
  the integration. **Missing required fields** are rejected.
- **Trailing data** after the JSON object is rejected.

## Responses

| Situation | HTTP status |
|---|---|
| Valid, verified `order.completed` event stored (or already stored) | `202 Accepted` |
| Valid, verified event of an unsupported type | `200 OK` (not stored) |
| Malformed or invalid payload | `422 Unprocessable Entity` (not stored) |

Authentication (request signing) is specified together with the intake
endpoint. Signatures are always verified before the payload is parsed.

## Loyalty outcome

Determined after storage by the worker, using the restaurant's loyalty program.
Precedence:

1. No `customer_id` → `no_customer`
2. Currency differs from the program's currency → `unsupported_currency`
3. Eligible amount below one earn unit → `below_minimum`
4. Otherwise → `awarded`, points = `floor(eligible / earn_unit) * points_per_unit`
