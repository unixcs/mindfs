import { appPath } from "./base";
import { protectedJSON } from "./api";

// 多设备 CLI 聚合: device management service.
// Endpoints: GET/POST/DELETE /api/devices, POST /api/devices/{id}/test|scan|sync.

export type Device = {
  id: string;
  name?: string;
  host: string;
  port?: number;
  user: string;
  identityFile?: string;
  watchRoots?: string[];
  mindfsHome?: string;
  pathPrefix?: string;
  autoCreateAgents?: boolean;
  enabled?: boolean;
  root_count?: number;
};

export type DeviceTestResult = {
  ok: boolean;
  home?: string;
  hostname?: string;
  latency_ms?: number;
  /** CLI name → absolute path; null means probed and absent. */
  clis?: Record<string, string | null>;
  provisioned?: string[];
  provision_error?: string;
  errors?: string[];
};

export type DeviceScanRoot = {
  path: string;
  name?: string;
};

export type DeviceScanResult = {
  roots: DeviceScanRoot[];
};

export type DeviceSyncResult = {
  imported: number;
  updated: number;
  skipped: number;
  errors?: string[];
  skipped_agents?: string[];
};

export type DeviceDeleteResult = {
  removed_agents: number;
  removed_roots: number;
};

export function deviceDisplayName(device: Device): string {
  return String(device.name || "").trim() || device.id;
}

export function deviceEndpoint(device: Device): string {
  return `${device.user}@${device.host}:${device.port || 22}`;
}

function normalizeDevice(input: unknown): Device | null {
  if (!input || typeof input !== "object") {
    return null;
  }
  const device = input as Device;
  const id = String(device.id || "").trim();
  const host = String(device.host || "").trim();
  const user = String(device.user || "").trim();
  if (!id || !host || !user) {
    return null;
  }
  return {
    ...device,
    id,
    host,
    user,
    port: Number.isFinite(device.port) && (device.port as number) > 0 ? device.port : 22,
    watchRoots: Array.isArray(device.watchRoots)
      ? device.watchRoots.map((item) => String(item || "").trim()).filter(Boolean)
      : [],
    name: typeof device.name === "string" ? device.name : undefined,
    identityFile: typeof device.identityFile === "string" ? device.identityFile : undefined,
    mindfsHome: typeof device.mindfsHome === "string" ? device.mindfsHome : undefined,
    pathPrefix: typeof device.pathPrefix === "string" ? device.pathPrefix : undefined,
    autoCreateAgents: !!device.autoCreateAgents,
    enabled: device.enabled !== false,
    root_count: Number.isFinite(device.root_count) ? device.root_count : 0,
  };
}

// Keep a tiny cache so several panels (selector, file tree, session list) can
// read the same list without hammering the endpoint.
const DEVICE_CACHE_TTL_MS = 30000;
let deviceCache: { at: number; devices: Device[] } | null = null;

export function invalidateDeviceCache(): void {
  deviceCache = null;
}

export async function fetchDevices(force = false): Promise<Device[]> {
  const cached = deviceCache;
  if (!force && cached && Date.now() - cached.at < DEVICE_CACHE_TTL_MS) {
    return cached.devices;
  }
  const data = await protectedJSON<{ devices?: unknown[] }>(appPath("/api/devices"));
  const items = Array.isArray(data?.devices) ? data.devices : [];
  const devices = items
    .map(normalizeDevice)
    .filter((item): item is Device => item !== null);
  deviceCache = { at: Date.now(), devices };
  return devices;
}

export async function saveDevice(device: Device): Promise<Device> {
  const saved = await protectedJSON<Device>(appPath("/api/devices"), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(device),
  });
  invalidateDeviceCache();
  return normalizeDevice(saved) || saved;
}

export async function deleteDevice(id: string): Promise<DeviceDeleteResult> {
  const result = await protectedJSON<DeviceDeleteResult>(
    appPath(`/api/devices/${encodeURIComponent(id)}`),
    { method: "DELETE" },
  );
  invalidateDeviceCache();
  return {
    removed_agents: Number(result?.removed_agents ?? 0) || 0,
    removed_roots: Number(result?.removed_roots ?? 0) || 0,
  };
}

export async function testDevice(id: string): Promise<DeviceTestResult> {
  return protectedJSON<DeviceTestResult>(appPath(`/api/devices/${encodeURIComponent(id)}/test`), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({}),
  });
}

export async function scanDevice(id: string): Promise<DeviceScanResult> {
  const data = await protectedJSON<DeviceScanResult>(appPath(`/api/devices/${encodeURIComponent(id)}/scan`), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({}),
  });
  return {
    roots: Array.isArray(data?.roots)
      ? data.roots
          .map((item) => ({
            path: String(item?.path || "").trim(),
            name: typeof item?.name === "string" ? item.name : undefined,
          }))
          .filter((item) => item.path)
      : [],
  };
}

export async function syncDevice(id: string, rootId?: string): Promise<DeviceSyncResult> {
  const result = await protectedJSON<DeviceSyncResult>(appPath(`/api/devices/${encodeURIComponent(id)}/sync`), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ root_id: String(rootId || "").trim() || undefined }),
  });
  return {
    imported: Number(result?.imported ?? 0) || 0,
    updated: Number(result?.updated ?? 0) || 0,
    skipped: Number(result?.skipped ?? 0) || 0,
    errors: Array.isArray(result?.errors) ? result.errors.map((item) => String(item)) : [],
    skipped_agents: Array.isArray(result?.skipped_agents)
      ? result.skipped_agents.map((item) => String(item))
      : [],
  };
}
