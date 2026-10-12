import React from "react";

// 多设备 CLI 聚合: small tag shown next to roots/agents/groups that belong to
// a remote device. Purely presentational — text is provided by the caller.
export function DeviceBadge({
  label,
  title,
  compact = false,
}: {
  label: string;
  title?: string;
  compact?: boolean;
}) {
  const trimmed = String(label || "").trim();
  if (!trimmed) {
    return null;
  }
  return (
    <span
      title={title || trimmed}
      style={{
        display: "inline-flex",
        alignItems: "center",
        maxWidth: compact ? "72px" : "120px",
        minWidth: 0,
        padding: "1px 6px",
        borderRadius: "999px",
        border: "1px solid color-mix(in srgb, var(--accent-color) 32%, transparent)",
        background: "color-mix(in srgb, var(--accent-color) 10%, transparent)",
        color: "var(--accent-color)",
        fontSize: "10px",
        fontWeight: 700,
        lineHeight: "14px",
        whiteSpace: "nowrap",
        overflow: "hidden",
        textOverflow: "ellipsis",
        flexShrink: 0,
        verticalAlign: "middle",
      }}
    >
      {trimmed}
    </span>
  );
}

// Filter chip used by the file tree / session list device bars. Styling
// follows the SessionFilterBar chip convention (26px pill, accent when active).
const deviceChipBaseStyle: React.CSSProperties = {
  height: "26px",
  border: "1px solid var(--border-color)",
  borderRadius: "999px",
  padding: "0 10px",
  display: "inline-flex",
  alignItems: "center",
  gap: "5px",
  background: "transparent",
  color: "var(--text-secondary)",
  fontSize: "12px",
  lineHeight: "1",
  whiteSpace: "nowrap",
  flexShrink: 0,
  cursor: "pointer",
  transition: "all 0.15s ease",
};

export function DeviceFilterChip({
  label,
  active,
  onClick,
  title,
}: {
  label: string;
  active: boolean;
  onClick: () => void;
  title?: string;
}) {
  return (
    <button
      type="button"
      aria-pressed={active}
      title={title || label}
      onClick={onClick}
      style={
        active
          ? {
              ...deviceChipBaseStyle,
              borderColor: "var(--accent-color)",
              color: "var(--accent-color)",
              background: "color-mix(in srgb, var(--accent-color) 12%, transparent)",
            }
          : deviceChipBaseStyle
      }
    >
      {label}
    </button>
  );
}
