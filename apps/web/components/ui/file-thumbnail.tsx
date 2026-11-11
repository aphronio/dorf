"use client";

import { useCallback, useSyncExternalStore } from "react";
import { cn } from "@/lib/utils";
import { useShape } from "@/lib/shape-context";

// ─── Object URLs ──────────────────────────────────────────────────────────
// One blob URL per File while any thumbnail shows it: subscribing creates it,
// the last unsubscribe revokes it. Created at subscription rather than in a
// memo, so StrictMode's simulated unmount/remount revokes the URL and then
// mints a fresh one, instead of leaving a revoked `blob:` URL on screen.
const objectUrls = new Map<File, { url: string; users: number }>();

function subscribeObjectUrl(file: File) {
  let entry = objectUrls.get(file);
  if (!entry) {
    entry = { url: URL.createObjectURL(file), users: 0 };
    objectUrls.set(file, entry);
  }
  entry.users += 1;
  const held = entry;
  return () => {
    held.users -= 1;
    if (held.users === 0) {
      URL.revokeObjectURL(held.url);
      objectUrls.delete(file);
    }
  };
}

function useObjectUrl(file: File | null): string | null {
  const subscribe = useCallback(
    () => (file ? subscribeObjectUrl(file) : () => {}),
    [file]
  );
  return useSyncExternalStore(
    subscribe,
    () => (file ? (objectUrls.get(file)?.url ?? null) : null),
    () => null
  );
}

// ─── File thumbnail ───────────────────────────────────────────────────────
// Read-only square preview of a File. Images use object-cover via
// `URL.createObjectURL`, with a spinner for the one frame before it exists;
// other files show a generic document icon. (Fluid's PDF preview is left out:
// Dorf attaches images only.) Self-contained (outline + surface + sizing) so
// it can be reused both inside the composer's preview row and to render
// already-sent attachments in a chat transcript.
interface FileThumbnailProps {
  file: File;
  /** Side length of the square thumbnail in pixels. */
  size: number;
  /** Explicit corner radius for a known nested inset. Defaults to the active
   *  shape's regular image/control radius. */
  radius?: number;
  className?: string;
}

function FileThumbnail({ file, size, radius, className }: FileThumbnailProps) {
  const shape = useShape();
  const isImage = file.type.startsWith("image/");

  // The URL exists from the commit on, so the first render has none. That
  // one-frame "before URL" state is covered by the bg-accent (no fallback
  // icon shown for images), so the transition is visually clean.
  const imageUrl = useObjectUrl(isImage ? file : null);

  const isPending = isImage && !imageUrl;

  return (
    <div
      className={cn(
        // Paint the hairline over the preview: unlike a border, it costs no
        // space, and pure neutral ink stays clean over every surface tint.
        "relative shrink-0 overflow-hidden bg-accent outline-1 -outline-offset-1 outline-black/10 dark:outline-white/10",
        shape.bg,
        className
      )}
      style={{
        width: size,
        height: size,
        // Keep className radius overrides working at the default. Inline
        // geometry is reserved for an explicit nested-radius calculation.
        ...(radius == null ? {} : { borderRadius: radius }),
      }}
    >
      {imageUrl ? (
        // eslint-disable-next-line @next/next/no-img-element
        <img
          src={imageUrl}
          alt={file.name}
          className="absolute inset-0 w-full h-full object-cover"
        />
      ) : isPending ? (
        // Circular spinner while we wait for the preview to be ready.
        // The thin ring is mostly subtle (border-border)
        // with one quadrant accented (border-t-muted-foreground) so the
        // `animate-spin` rotation reads as a moving arc.
        <div className="absolute inset-0 flex items-center justify-center">
          <div
            className="w-6 h-6 rounded-full border-2 border-border border-t-muted-foreground animate-spin"
            aria-label="Loading preview"
            role="status"
          />
        </div>
      ) : (
        // Generic document glyph for files with no renderable preview.
        // Inline SVG (not the icon system) so the thumbnail stays
        // self-contained for registry consumers.
        <div
          className="absolute inset-0 flex items-center justify-center text-muted-foreground"
          role="img"
          aria-label={file.name}
        >
          <svg
            width={Math.max(16, size * 0.35)}
            height={Math.max(16, size * 0.35)}
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z" />
            <path d="M14 3v5h5" />
          </svg>
        </div>
      )}
    </div>
  );
}

export { FileThumbnail, useObjectUrl };
export type { FileThumbnailProps };
