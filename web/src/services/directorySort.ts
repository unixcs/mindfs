export type FileEntry = {
  name: string;
  path: string;
  is_dir: boolean;
  is_symlink?: boolean;
  is_root?: boolean;
  size?: number;
  mtime?: string;
  /** Owning remote device id for device roots; empty/undefined for local roots. */
  device?: string;
};

export type DirectorySortMode =
  | "name-asc"
  | "name-desc"
  | "mtime-desc"
  | "mtime-asc"
  | "size-desc"
  | "size-asc";

export const DEFAULT_DIRECTORY_SORT_MODE: DirectorySortMode = "name-asc";

export const DIRECTORY_SORT_OPTIONS: Array<{ value: DirectorySortMode }> = [
  { value: "name-asc" },
  { value: "name-desc" },
  { value: "mtime-desc" },
  { value: "mtime-asc" },
  { value: "size-desc" },
  { value: "size-asc" },
];

export function sortDirectoryEntries(entries: FileEntry[], mode: DirectorySortMode): FileEntry[] {
  return [...entries].sort((left, right) => {
    if (left.is_dir !== right.is_dir) {
      return left.is_dir ? -1 : 1;
    }

    if (mode === "mtime-desc" || mode === "mtime-asc") {
      const leftTime = Date.parse(left.mtime || "") || 0;
      const rightTime = Date.parse(right.mtime || "") || 0;
      const diff = mode === "mtime-desc" ? rightTime - leftTime : leftTime - rightTime;
      if (diff !== 0) {
        return diff;
      }
    }

    if (mode === "size-desc" || mode === "size-asc") {
      const leftSize = left.size || 0;
      const rightSize = right.size || 0;
      const diff = mode === "size-desc" ? rightSize - leftSize : leftSize - rightSize;
      if (diff !== 0) {
        return diff;
      }
    }

    const direction = mode === "name-desc" ? -1 : 1;
    return left.name.localeCompare(right.name, undefined, {
      numeric: true,
      sensitivity: "base",
    }) * direction;
  });
}
