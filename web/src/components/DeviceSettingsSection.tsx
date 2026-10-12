import React from "react";
import {
  deleteDevice,
  deviceDisplayName,
  deviceEndpoint,
  saveDevice,
  scanDevice,
  syncDevice,
  testDevice,
  type Device,
  type DeviceScanResult,
  type DeviceSyncResult,
  type DeviceTestResult,
} from "../services/devices";
import { useI18n } from "../i18n";

// 多设备 CLI 聚合: "远程设备" settings section (opened from the FileTree
// hamburger menu). Device CRUD + test/scan/sync actions + the single/multi
// device display switch. All state lives here; parents only refresh lists.

const DEVICE_ID_PATTERN = /^[a-z0-9][a-z0-9_-]{0,63}$/;

type DeviceDraft = {
  id: string;
  name: string;
  host: string;
  port: string;
  user: string;
  identityFile: string;
  watchRoots: string;
  mindfsHome: string;
  pathPrefix: string;
  autoCreateAgents: boolean;
  enabled: boolean;
};

const EMPTY_DRAFT: DeviceDraft = {
  id: "",
  name: "",
  host: "",
  port: "22",
  user: "",
  identityFile: "",
  watchRoots: "",
  mindfsHome: "",
  pathPrefix: "",
  autoCreateAgents: true,
  enabled: true,
};

function draftFromDevice(device: Device): DeviceDraft {
  return {
    id: device.id,
    name: String(device.name || ""),
    host: String(device.host || ""),
    port: String(device.port || 22),
    user: String(device.user || ""),
    identityFile: String(device.identityFile || ""),
    watchRoots: (device.watchRoots || []).join("\n"),
    mindfsHome: String(device.mindfsHome || ""),
    pathPrefix: String(device.pathPrefix || ""),
    autoCreateAgents: device.autoCreateAgents !== false,
    enabled: device.enabled !== false,
  };
}

type DeviceSettingsSectionProps = {
  open: boolean;
  devices: Device[];
  /** device id → number of provisioned device agents (for delete confirm). */
  agentCountByDevice: Record<string, number>;
  multiDeviceViewEnabled: boolean;
  onMultiDeviceViewChange: (enabled: boolean) => void;
  /** Re-fetch the device list after a mutation. */
  onDevicesChanged: () => void;
  /** Register a scanned remote path as a managed root (existing add-dir flow). */
  onAddDeviceRoot: (device: Device, path: string) => Promise<void>;
  onCancel: () => void;
  onEditingChange?: (editing: boolean) => void;
};

const panelStyle: React.CSSProperties = {
  width: "100%",
  maxHeight: "min(640px, calc(100vh - 58px))",
  padding: "10px",
  borderRadius: "12px",
  border: "1px solid var(--border-color)",
  background: "var(--menu-bg)",
  boxShadow: "0 12px 30px rgba(15, 23, 42, 0.14)",
  display: "flex",
  flexDirection: "column",
  gap: "10px",
  boxSizing: "border-box",
  overflow: "hidden",
};

const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  gap: "8px",
  flexShrink: 0,
};

const titleStyle: React.CSSProperties = {
  fontSize: "12px",
  fontWeight: 700,
  color: "var(--text-primary)",
};

const inputStyle: React.CSSProperties = {
  width: "100%",
  borderRadius: "8px",
  border: "1px solid var(--border-color)",
  background: "transparent",
  color: "var(--text-primary)",
  fontSize: "12px",
  padding: "7px 9px",
  outline: "none",
  boxSizing: "border-box",
};

const labelStyle: React.CSSProperties = {
  display: "block",
  fontSize: "11px",
  fontWeight: 600,
  color: "var(--text-secondary)",
  marginBottom: "4px",
};

const primaryButtonStyle = (disabled: boolean): React.CSSProperties => ({
  border: "none",
  background: "var(--accent-color)",
  color: "#fff",
  borderRadius: "8px",
  padding: "6px 12px",
  fontSize: "12px",
  fontWeight: 600,
  cursor: disabled ? "not-allowed" : "pointer",
  opacity: disabled ? 0.6 : 1,
});

const secondaryButtonStyle = (disabled: boolean): React.CSSProperties => ({
  border: "1px solid var(--border-color)",
  background: "transparent",
  color: disabled ? "var(--text-secondary)" : "var(--text-primary)",
  borderRadius: "8px",
  padding: "6px 12px",
  fontSize: "12px",
  fontWeight: 600,
  cursor: disabled ? "default" : "pointer",
});

const actionChipStyle = (disabled: boolean): React.CSSProperties => ({
  border: "1px solid var(--border-color)",
  background: "transparent",
  color: disabled ? "var(--text-secondary)" : "var(--text-primary)",
  borderRadius: "7px",
  padding: "3px 8px",
  fontSize: "11px",
  fontWeight: 600,
  whiteSpace: "nowrap",
  cursor: disabled ? "default" : "pointer",
});

const resultBoxStyle: React.CSSProperties = {
  padding: "8px 10px",
  borderRadius: "8px",
  background: "rgba(0, 0, 0, 0.03)",
  border: "1px solid var(--border-color)",
  fontSize: "11px",
  lineHeight: 1.6,
  color: "var(--text-primary)",
  whiteSpace: "normal",
  overflowWrap: "anywhere",
  wordBreak: "break-word",
};

const errorTextStyle: React.CSSProperties = {
  color: "#dc2626",
  fontSize: "11px",
  lineHeight: 1.5,
  whiteSpace: "normal",
  overflowWrap: "anywhere",
  wordBreak: "break-word",
};

export function DeviceSettingsSection({
  open,
  devices,
  agentCountByDevice,
  multiDeviceViewEnabled,
  onMultiDeviceViewChange,
  onDevicesChanged,
  onAddDeviceRoot,
  onCancel,
  onEditingChange,
}: DeviceSettingsSectionProps) {
  const { t } = useI18n();
  const [draft, setDraft] = React.useState<DeviceDraft | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [notice, setNotice] = React.useState("");
  const [testResults, setTestResults] = React.useState<Record<string, DeviceTestResult | "pending">>({});
  const [scanResults, setScanResults] = React.useState<Record<string, DeviceScanResult | "pending">>({});
  const [syncResults, setSyncResults] = React.useState<Record<string, DeviceSyncResult | "pending">>({});
  const [addingRootPath, setAddingRootPath] = React.useState("");

  const editDraft = React.useCallback((nextDraft: DeviceDraft | null) => {
    setDraft(nextDraft);
    onEditingChange?.(nextDraft !== null);
  }, [onEditingChange]);

  React.useEffect(() => {
    if (!open) {
      editDraft(null);
      setError("");
      setNotice("");
      setTestResults({});
      setScanResults({});
      setSyncResults({});
      setAddingRootPath("");
    }
  }, [editDraft, open]);

  const draftError = (() => {
    if (!draft) {
      return "";
    }
    const id = draft.id.trim();
    const host = draft.host.trim();
    const user = draft.user.trim();
    if (!id) {
      return t("devices.errorIdRequired");
    }
    if (!DEVICE_ID_PATTERN.test(id)) {
      return t("devices.errorIdPattern");
    }
    if (!host) {
      return t("devices.errorHostRequired");
    }
    if (!user) {
      return t("devices.errorUserRequired");
    }
    const port = Number(draft.port.trim() || "22");
    if (!Number.isInteger(port) || port <= 0 || port > 65535) {
      return t("devices.errorPortInvalid");
    }
    return "";
  })();

  if (!open) {
    return null;
  }

  const submitDraft = async () => {
    if (!draft || busy || draftError) {
      return;
    }
    setBusy(true);
    setError("");
    setNotice("");
    try {
      await saveDevice({
        id: draft.id.trim(),
        name: draft.name.trim(),
        host: draft.host.trim(),
        port: Number(draft.port.trim() || "22"),
        user: draft.user.trim(),
        identityFile: draft.identityFile.trim(),
        watchRoots: draft.watchRoots
          .split(/[\n,]/)
          .map((item) => item.trim())
          .filter(Boolean),
        mindfsHome: draft.mindfsHome.trim(),
        pathPrefix: draft.pathPrefix.trim(),
        autoCreateAgents: draft.autoCreateAgents,
        enabled: draft.enabled,
      });
      editDraft(null);
      onDevicesChanged();
    } catch (err) {
      setError(err instanceof Error ? err.message : t("devices.saveFailed"));
    } finally {
      setBusy(false);
    }
  };

  const runTest = async (device: Device) => {
    if (busy) {
      return;
    }
    setBusy(true);
    setError("");
    setNotice("");
    setTestResults((prev) => ({ ...prev, [device.id]: "pending" }));
    try {
      const result = await testDevice(device.id);
      setTestResults((prev) => ({ ...prev, [device.id]: result }));
      if (result.ok && result.provisioned?.length) {
        onDevicesChanged();
      }
    } catch (err) {
      setTestResults((prev) => {
        const next = { ...prev };
        delete next[device.id];
        return next;
      });
      setError(err instanceof Error ? err.message : t("devices.testFailed"));
    } finally {
      setBusy(false);
    }
  };

  const runScan = async (device: Device) => {
    if (busy) {
      return;
    }
    setBusy(true);
    setError("");
    setNotice("");
    setScanResults((prev) => ({ ...prev, [device.id]: "pending" }));
    try {
      const result = await scanDevice(device.id);
      setScanResults((prev) => ({ ...prev, [device.id]: result }));
    } catch (err) {
      setScanResults((prev) => {
        const next = { ...prev };
        delete next[device.id];
        return next;
      });
      setError(err instanceof Error ? err.message : t("devices.scanFailed"));
    } finally {
      setBusy(false);
    }
  };

  const runSync = async (device: Device) => {
    if (busy) {
      return;
    }
    setBusy(true);
    setError("");
    setNotice("");
    setSyncResults((prev) => ({ ...prev, [device.id]: "pending" }));
    try {
      const result = await syncDevice(device.id);
      setSyncResults((prev) => ({ ...prev, [device.id]: result }));
      // 同步可能导入镜像会话；触发应用层刷新让分组会话列表立即出现新会话
      onDevicesChanged();
    } catch (err) {
      setSyncResults((prev) => {
        const next = { ...prev };
        delete next[device.id];
        return next;
      });
      setError(err instanceof Error ? err.message : t("devices.syncFailed"));
    } finally {
      setBusy(false);
    }
  };

  const removeDevice = async (device: Device) => {
    if (busy) {
      return;
    }
    const agentCount = agentCountByDevice[device.id] || 0;
    const rootCount = device.root_count || 0;
    const confirmed = window.confirm(
      t("devices.confirmDelete", {
        name: deviceDisplayName(device),
        agents: String(agentCount),
        roots: String(rootCount),
      }),
    );
    if (!confirmed) {
      return;
    }
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const result = await deleteDevice(device.id);
      setTestResults((prev) => {
        const next = { ...prev };
        delete next[device.id];
        return next;
      });
      setScanResults((prev) => {
        const next = { ...prev };
        delete next[device.id];
        return next;
      });
      setSyncResults((prev) => {
        const next = { ...prev };
        delete next[device.id];
        return next;
      });
      setNotice(
        t("devices.deleteDone", {
          agents: String(result.removed_agents),
          roots: String(result.removed_roots),
        }),
      );
      onDevicesChanged();
    } catch (err) {
      setError(err instanceof Error ? err.message : t("devices.deleteFailed"));
    } finally {
      setBusy(false);
    }
  };

  const addRoot = async (device: Device, path: string) => {
    if (busy || addingRootPath) {
      return;
    }
    setAddingRootPath(path);
    setError("");
    setNotice("");
    try {
      await onAddDeviceRoot(device, path);
      setNotice(t("devices.rootAdded", { path }));
    } catch (err) {
      setError(err instanceof Error ? err.message : t("devices.rootAddFailed"));
    } finally {
      setAddingRootPath("");
    }
  };

  return (
    <section style={panelStyle} data-mindfs-device-settings="1">
      {!draft ? (
        <>
          <header style={headerStyle}>
            <div style={titleStyle}>{t("devices.title")}</div>
            <button
              type="button"
              aria-label={t("devices.add")}
              title={t("devices.add")}
              disabled={busy}
              onClick={() => editDraft({ ...EMPTY_DRAFT })}
              style={primaryButtonStyle(busy)}
            >
              {t("devices.add")}
            </button>
          </header>

          <div
            style={{
              display: "flex",
              alignItems: "center",
              justifyContent: "space-between",
              gap: "8px",
              padding: "8px 10px",
              borderRadius: "8px",
              border: "1px solid var(--border-color)",
              flexShrink: 0,
            }}
          >
            <div style={{ minWidth: 0 }}>
              <div style={{ fontSize: "12px", fontWeight: 600, color: "var(--text-primary)" }}>
                {t("devices.multiDeviceView")}
              </div>
              <div style={{ fontSize: "11px", color: "var(--text-secondary)", marginTop: "2px" }}>
                {t("devices.multiDeviceViewHint")}
              </div>
            </div>
            <div style={{ display: "inline-flex", gap: "4px", flexShrink: 0 }}>
              {([false, true] as const).map((value) => {
                const active = multiDeviceViewEnabled === value;
                return (
                  <button
                    key={String(value)}
                    type="button"
                    aria-pressed={active}
                    onClick={() => onMultiDeviceViewChange(value)}
                    style={{
                      ...actionChipStyle(false),
                      borderColor: active ? "var(--accent-color)" : "var(--border-color)",
                      color: active ? "var(--accent-color)" : "var(--text-secondary)",
                      background: active
                        ? "color-mix(in srgb, var(--accent-color) 12%, transparent)"
                        : "transparent",
                    }}
                  >
                    {t(value ? "devices.viewMulti" : "devices.viewSingle")}
                  </button>
                );
              })}
            </div>
          </div>

          <div style={{ flex: 1, minHeight: 0, overflow: "auto", display: "flex", flexDirection: "column", gap: "8px" }}>
            {devices.length === 0 ? (
              <div style={{ ...resultBoxStyle, color: "var(--text-secondary)" }}>
                {t("devices.empty")}
              </div>
            ) : null}
            {devices.map((device) => {
              const testResult = testResults[device.id];
              const scanResult = scanResults[device.id];
              const syncResult = syncResults[device.id];
              return (
                <div
                  key={device.id}
                  style={{
                    padding: "10px",
                    borderRadius: "10px",
                    border: "1px solid var(--border-color)",
                    display: "flex",
                    flexDirection: "column",
                    gap: "8px",
                  }}
                >
                  <div style={{ display: "flex", alignItems: "center", gap: "8px", minWidth: 0 }}>
                    <span
                      aria-hidden="true"
                      style={{
                        width: "8px",
                        height: "8px",
                        borderRadius: "50%",
                        flexShrink: 0,
                        background: device.enabled !== false ? "#22c55e" : "#94a3b8",
                      }}
                    />
                    <div style={{ flex: 1, minWidth: 0 }}>
                      <div
                        style={{
                          fontSize: "12px",
                          fontWeight: 700,
                          color: "var(--text-primary)",
                          overflow: "hidden",
                          textOverflow: "ellipsis",
                          whiteSpace: "nowrap",
                        }}
                      >
                        {deviceDisplayName(device)}
                      </div>
                      <div
                        style={{
                          fontSize: "11px",
                          color: "var(--text-secondary)",
                          overflow: "hidden",
                          textOverflow: "ellipsis",
                          whiteSpace: "nowrap",
                        }}
                      >
                        {deviceEndpoint(device)}
                        {" · "}
                        {t("devices.rootCount", { count: device.root_count || 0 })}
                      </div>
                    </div>
                    <button
                      type="button"
                      aria-pressed={device.enabled !== false}
                      title={t(device.enabled !== false ? "devices.enabled" : "devices.disabled")}
                      onClick={async () => {
                        if (busy) return;
                        setBusy(true);
                        setError("");
                        try {
                          await saveDevice({ ...device, enabled: device.enabled === false });
                          onDevicesChanged();
                        } catch (err) {
                          setError(err instanceof Error ? err.message : t("devices.saveFailed"));
                        } finally {
                          setBusy(false);
                        }
                      }}
                      style={{
                        ...actionChipStyle(busy),
                        borderColor: device.enabled !== false ? "var(--accent-color)" : "var(--border-color)",
                        color: device.enabled !== false ? "var(--accent-color)" : "var(--text-secondary)",
                      }}
                    >
                      {t(device.enabled !== false ? "devices.enabled" : "devices.disabled")}
                    </button>
                  </div>

                  <div style={{ display: "flex", alignItems: "center", gap: "6px", flexWrap: "wrap" }}>
                    <button type="button" disabled={busy} onClick={() => editDraft(draftFromDevice(device))} style={actionChipStyle(busy)}>
                      {t("devices.edit")}
                    </button>
                    <button type="button" disabled={busy || testResult === "pending"} onClick={() => void runTest(device)} style={actionChipStyle(busy || testResult === "pending")}>
                      {testResult === "pending" ? t("devices.testing") : t("devices.test")}
                    </button>
                    <button type="button" disabled={busy || scanResult === "pending"} onClick={() => void runScan(device)} style={actionChipStyle(busy || scanResult === "pending")}>
                      {scanResult === "pending" ? t("devices.scanning") : t("devices.scan")}
                    </button>
                    <button
                      type="button"
                      disabled={busy || syncResult === "pending" || !String(device.mindfsHome || "").trim()}
                      title={String(device.mindfsHome || "").trim() ? undefined : t("devices.syncNeedsMindfsHome")}
                      onClick={() => void runSync(device)}
                      style={actionChipStyle(busy || syncResult === "pending" || !String(device.mindfsHome || "").trim())}
                    >
                      {syncResult === "pending" ? t("devices.syncing") : t("devices.sync")}
                    </button>
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => void removeDevice(device)}
                      style={{ ...actionChipStyle(busy), color: busy ? "var(--text-secondary)" : "#dc2626", borderColor: busy ? "var(--border-color)" : "color-mix(in srgb, #dc2626 40%, transparent)" }}
                    >
                      {t("devices.delete")}
                    </button>
                  </div>

                  {testResult && testResult !== "pending" ? (
                    <div style={resultBoxStyle}>
                      {testResult.ok ? (
                        <div style={{ color: "#16a34a", fontWeight: 700 }}>{t("devices.testOk")}</div>
                      ) : (
                        <div style={{ color: "#dc2626", fontWeight: 700 }}>{t("devices.testFailedResult")}</div>
                      )}
                      {testResult.hostname ? <div>{t("devices.testHostname", { hostname: testResult.hostname })}</div> : null}
                      {testResult.home ? <div>{t("devices.testHome", { home: testResult.home })}</div> : null}
                      {typeof testResult.latency_ms === "number" ? (
                        <div>{t("devices.testLatency", { latency: testResult.latency_ms })}</div>
                      ) : null}
                      {testResult.clis && Object.keys(testResult.clis).length > 0 ? (
                        <div>
                          {Object.entries(testResult.clis).map(([cli, path]) => (
                            <div key={cli}>
                              {cli === "qodercli" ? "qoder" : cli}: {path ? String(path) : t("devices.cliMissing")}
                            </div>
                          ))}
                        </div>
                      ) : null}
                      {testResult.provisioned && testResult.provisioned.length > 0 ? (
                        <div>{t("devices.provisioned", { agents: testResult.provisioned.join(", ") })}</div>
                      ) : null}
                      {testResult.provision_error ? <div style={errorTextStyle}>{testResult.provision_error}</div> : null}
                      {testResult.errors && testResult.errors.length > 0 ? (
                        <div style={errorTextStyle}>{testResult.errors.join("\n")}</div>
                      ) : null}
                    </div>
                  ) : null}

                  {scanResult && scanResult !== "pending" ? (
                    <div style={resultBoxStyle}>
                      {scanResult.roots.length === 0 ? (
                        <div style={{ color: "var(--text-secondary)" }}>{t("devices.scanEmpty")}</div>
                      ) : (
                        scanResult.roots.map((root) => {
                          const added = addingRootPath === root.path;
                          return (
                            <div
                              key={root.path}
                              style={{ display: "flex", alignItems: "center", gap: "8px", padding: "3px 0", minWidth: 0 }}
                            >
                              <span
                                title={root.path}
                                style={{ flex: 1, minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}
                              >
                                {root.name || root.path}
                              </span>
                              <button
                                type="button"
                                disabled={busy || !!addingRootPath}
                                onClick={() => void addRoot(device, root.path)}
                                style={actionChipStyle(busy || !!addingRootPath)}
                              >
                                {added ? t("devices.addingRoot") : t("devices.addRoot")}
                              </button>
                            </div>
                          );
                        })
                      )}
                    </div>
                  ) : null}

                  {syncResult && syncResult !== "pending" ? (
                    <div style={resultBoxStyle}>
                      <div>
                        {t("devices.syncResult", {
                          imported: syncResult.imported,
                          updated: syncResult.updated,
                          skipped: syncResult.skipped,
                        })}
                      </div>
                      {syncResult.skipped_agents && syncResult.skipped_agents.length > 0 ? (
                        <div style={{ color: "var(--text-secondary)" }}>
                          {t("devices.syncSkippedAgents", { agents: syncResult.skipped_agents.join(", ") })}
                        </div>
                      ) : null}
                      {syncResult.errors && syncResult.errors.length > 0 ? (
                        <div style={errorTextStyle}>{syncResult.errors.join("\n")}</div>
                      ) : null}
                    </div>
                  ) : null}
                </div>
              );
            })}
          </div>
        </>
      ) : (
        <>
          <header style={headerStyle}>
            <div style={titleStyle}>{t(draft.id && devices.some((item) => item.id === draft.id) ? "devices.editTitle" : "devices.addTitle")}</div>
          </header>
          <div style={{ flex: 1, minHeight: 0, overflow: "auto", display: "flex", flexDirection: "column", gap: "10px" }}>
            <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: "8px" }}>
              <div>
                <label style={labelStyle}>{t("devices.fieldId")}</label>
                <input
                  value={draft.id}
                  disabled={busy || devices.some((item) => item.id === draft.id)}
                  onChange={(event) => setDraft((prev) => (prev ? { ...prev, id: event.target.value } : prev))}
                  placeholder="nas"
                  style={{ ...inputStyle, opacity: devices.some((item) => item.id === draft.id) ? 0.6 : 1 }}
                />
              </div>
              <div>
                <label style={labelStyle}>{t("devices.fieldName")}</label>
                <input
                  value={draft.name}
                  disabled={busy}
                  onChange={(event) => setDraft((prev) => (prev ? { ...prev, name: event.target.value } : prev))}
                  placeholder="NAS"
                  style={inputStyle}
                />
              </div>
            </div>
            <div style={{ display: "grid", gridTemplateColumns: "2fr 1fr", gap: "8px" }}>
              <div>
                <label style={labelStyle}>{t("devices.fieldHost")}</label>
                <input
                  value={draft.host}
                  disabled={busy}
                  onChange={(event) => setDraft((prev) => (prev ? { ...prev, host: event.target.value } : prev))}
                  placeholder="100.118.126.86"
                  style={inputStyle}
                />
              </div>
              <div>
                <label style={labelStyle}>{t("devices.fieldPort")}</label>
                <input
                  value={draft.port}
                  disabled={busy}
                  inputMode="numeric"
                  onChange={(event) => setDraft((prev) => (prev ? { ...prev, port: event.target.value } : prev))}
                  style={inputStyle}
                />
              </div>
            </div>
            <div>
              <label style={labelStyle}>{t("devices.fieldUser")}</label>
              <input
                value={draft.user}
                disabled={busy}
                onChange={(event) => setDraft((prev) => (prev ? { ...prev, user: event.target.value } : prev))}
                placeholder="root"
                style={inputStyle}
              />
            </div>
            <div>
              <label style={labelStyle}>{t("devices.fieldIdentityFile")}</label>
              <input
                value={draft.identityFile}
                disabled={busy}
                onChange={(event) => setDraft((prev) => (prev ? { ...prev, identityFile: event.target.value } : prev))}
                placeholder="~/.ssh/id_ed25519"
                style={inputStyle}
              />
            </div>
            <div>
              <label style={labelStyle}>{t("devices.fieldWatchRoots")}</label>
              <textarea
                value={draft.watchRoots}
                disabled={busy}
                onChange={(event) => setDraft((prev) => (prev ? { ...prev, watchRoots: event.target.value } : prev))}
                placeholder={"/mnt/0\n/root/projects"}
                rows={3}
                style={{ ...inputStyle, resize: "vertical", lineHeight: 1.5 }}
              />
            </div>
            <div>
              <label style={labelStyle}>{t("devices.fieldMindfsHome")}</label>
              <input
                value={draft.mindfsHome}
                disabled={busy}
                onChange={(event) => setDraft((prev) => (prev ? { ...prev, mindfsHome: event.target.value } : prev))}
                placeholder="/opt/mindfs-data"
                style={inputStyle}
              />
            </div>
            <div>
              <label style={labelStyle}>{t("devices.fieldPathPrefix")}</label>
              <input
                value={draft.pathPrefix}
                disabled={busy}
                onChange={(event) => setDraft((prev) => (prev ? { ...prev, pathPrefix: event.target.value } : prev))}
                placeholder="export PATH=$HOME/.opencode/bin:$PATH"
                style={inputStyle}
              />
            </div>
            <label style={{ display: "flex", alignItems: "center", gap: "8px", fontSize: "12px", color: "var(--text-primary)", cursor: busy ? "default" : "pointer" }}>
              <input
                type="checkbox"
                checked={draft.autoCreateAgents}
                disabled={busy}
                onChange={(event) => setDraft((prev) => (prev ? { ...prev, autoCreateAgents: event.target.checked } : prev))}
              />
              <span>{t("devices.fieldAutoCreateAgents")}</span>
            </label>
            <label style={{ display: "flex", alignItems: "center", gap: "8px", fontSize: "12px", color: "var(--text-primary)", cursor: busy ? "default" : "pointer" }}>
              <input
                type="checkbox"
                checked={draft.enabled}
                disabled={busy}
                onChange={(event) => setDraft((prev) => (prev ? { ...prev, enabled: event.target.checked } : prev))}
              />
              <span>{t("devices.fieldEnabled")}</span>
            </label>
          </div>
          {draftError ? <div style={errorTextStyle}>{draftError}</div> : null}
          <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", flexShrink: 0 }}>
            <button type="button" disabled={busy} onClick={() => editDraft(null)} style={secondaryButtonStyle(busy)}>
              {t("common.cancel")}
            </button>
            <button type="button" disabled={busy || !!draftError} onClick={() => void submitDraft()} style={primaryButtonStyle(busy || !!draftError)}>
              {busy ? t("devices.saving") : t("common.save")}
            </button>
          </div>
        </>
      )}

      {error ? <div style={{ ...errorTextStyle, flexShrink: 0 }}>{error}</div> : null}
      {notice ? (
        <div style={{ ...resultBoxStyle, color: "var(--text-secondary)", flexShrink: 0 }}>{notice}</div>
      ) : null}
    </section>
  );
}
