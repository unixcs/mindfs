export type FontSizeRegion = "fileSidebar" | "main" | "sessionSidebar";

export type FontSizePreferences = Record<FontSizeRegion, number>;

export const DEFAULT_FONT_SIZE_PREFERENCES: FontSizePreferences = {
  fileSidebar: 1,
  main: 1,
  sessionSidebar: 1,
};

export const FONT_SIZE_MIN = 0.8;
export const FONT_SIZE_MAX = 1.4;
export const FONT_SIZE_STEP = 0.1;

const FONT_SIZE_STORAGE_KEY = "mindfs-font-size-preferences";

function normalizeScale(value: unknown): number {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    return 1;
  }
  const stepped = Math.round(value / FONT_SIZE_STEP) * FONT_SIZE_STEP;
  return Math.min(FONT_SIZE_MAX, Math.max(FONT_SIZE_MIN, Number(stepped.toFixed(1))));
}

export function loadFontSizePreferences(): FontSizePreferences {
  if (typeof window === "undefined") {
    return DEFAULT_FONT_SIZE_PREFERENCES;
  }
  try {
    return parseFontSizePreferences(window.localStorage.getItem(FONT_SIZE_STORAGE_KEY) || "{}") ?? DEFAULT_FONT_SIZE_PREFERENCES;
  } catch {
    return DEFAULT_FONT_SIZE_PREFERENCES;
  }
}

// parseFontSizePreferences normalizes an unknown value (localStorage JSON or
// a server-replicated blob). Returns null when the value is not a usable
// object, so callers can distinguish "absent" from "partial".
export function parseFontSizePreferences(value: unknown): FontSizePreferences | null {
  let input = value;
  if (typeof input === "string") {
    try {
      input = JSON.parse(input);
    } catch {
      return null;
    }
  }
  if (!input || typeof input !== "object" || Array.isArray(input)) {
    return null;
  }
  const record = input as Partial<Record<FontSizeRegion, unknown>>;
  return {
    fileSidebar: normalizeScale(record.fileSidebar),
    main: normalizeScale(record.main),
    sessionSidebar: normalizeScale(record.sessionSidebar),
  };
}

export function persistFontSizePreferences(preferences: FontSizePreferences): void {
  if (typeof window === "undefined") {
    return;
  }
  try {
    window.localStorage.setItem(FONT_SIZE_STORAGE_KEY, JSON.stringify(preferences));
  } catch {
    // The current session can still use the selected sizes when storage is unavailable.
  }
}

export function changeFontSize(
  preferences: FontSizePreferences,
  region: FontSizeRegion,
  direction: -1 | 1,
): FontSizePreferences {
  return {
    ...preferences,
    [region]: normalizeScale(preferences[region] + direction * FONT_SIZE_STEP),
  };
}
