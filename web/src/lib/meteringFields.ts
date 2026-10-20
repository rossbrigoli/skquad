// S-269: shared metering form fields for resource registration/edit.
//
// Metering is OPTIONAL per resource (Ross's hard requirement): the
// toggle defaults OFF; a non-metered resource needs no metering
// config and every call to it costs 0. The price textbox is only
// meaningful (and only rendered) while the toggle is on, and a
// metered resource must carry a price >= 0 — mirrored server-side.

export type MeteringFormFields = {
  metered: boolean;
  pricePerCall: string;
};

// meteringFromResource prefills the metering fields from a registry
// row (edit flow).
export function meteringFromResource(resource: {
  metered?: boolean;
  price_per_call?: number | null;
}): MeteringFormFields {
  return {
    metered: resource.metered ?? false,
    pricePerCall: resource.price_per_call != null ? String(resource.price_per_call) : "",
  };
}

// validateMeteringFields returns a human-readable error or null.
export function validateMeteringFields(fields: MeteringFormFields): string | null {
  if (!fields.metered) return null;
  const price = Number(fields.pricePerCall.trim());
  if (fields.pricePerCall.trim() === "" || !Number.isFinite(price) || price < 0) {
    return "Metered price per call must be a number ≥ 0";
  }
  return null;
}

// meteringPayload renders the CP wire fields: always `metered`, plus
// `price_per_call` when metered.
export function meteringPayload(fields: MeteringFormFields): Record<string, unknown> {
  const payload: Record<string, unknown> = { metered: fields.metered };
  if (fields.metered) {
    payload.price_per_call = Number(fields.pricePerCall.trim());
  }
  return payload;
}
