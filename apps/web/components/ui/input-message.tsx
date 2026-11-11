"use client";

import {
  forwardRef,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ChangeEvent,
  type ClipboardEvent as ReactClipboardEvent,
  type DragEvent as ReactDragEvent,
  type HTMLAttributes,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
  type TextareaHTMLAttributes,
} from "react";
import { AnimatePresence, motion, useReducedMotion } from "motion/react";
import { cn } from "@/lib/utils";
import { fontWeights } from "@/lib/font-weight";
import { spring } from "@/lib/springs";
import { omit } from "@/lib/omit";
import { nestedRadius, useShape } from "@/lib/shape-context";
import { SizeProvider, useSize, fieldTouchClass, type SizeVariant } from "@/lib/size-context";
import { useIcons } from "@/lib/icon-context";
import { surfaceClasses } from "@/lib/surface-classes";
import { SurfaceProvider } from "@/lib/surface-context";
import { Button } from "@/components/ui/button";
import { FileThumbnail } from "@/components/ui/file-thumbnail";
import { Tooltip } from "@/components/ui/tooltip";
import { useTouchPrimary } from "@/hooks/use-touch-primary";

const useIsoLayoutEffect =
  typeof window !== "undefined" ? useLayoutEffect : useEffect;

/**
 * Measured layout height for one of the composer's collapsible regions
 * (attachments, queue, suggestions).
 *
 * These animate to a self-measured PIXEL height rather than `height: "auto"`:
 * framer resolves an "auto" target from the element's *visual* (transformed)
 * size, so under a scaled ancestor — the /demo card scales its slide — the
 * region springs out to scale× its real height and snaps back when "auto"
 * lands, which reads as the region ballooning and then correcting. Same
 * treatment as Accordion's content height.
 *
 * Returns a ref for the region's CONTENT element. The height it reports is the
 * clipping parent's scrollHeight, so inner margins count (the parent's
 * overflow-hidden makes it a block formatting context, so they don't collapse
 * out) and the value stays correct while the animated height is mid-flight.
 * Observing the child rather than the parent keeps the ResizeObserver out of a
 * feedback loop with that animation.
 */
function useRegionHeight() {
  const roRef = useRef<ResizeObserver | null>(null);
  const [height, setHeight] = useState<number | null>(null);
  const ref = useCallback((el: HTMLElement | null) => {
    roRef.current?.disconnect();
    roRef.current = null;
    if (!el) return;
    const sync = () => {
      const next = el.parentElement?.scrollHeight ?? el.offsetHeight;
      if (next > 0) setHeight(next);
    };
    sync();
    const ro = new ResizeObserver(sync);
    ro.observe(el);
    roRef.current = ro;
  }, []);
  return [ref, height] as const;
}

const DEFAULT_ACCEPT = "image/png,image/jpeg,application/pdf";

interface InputMessageSlotContext {
  /** Opens the native file picker via the hidden `<input type="file">`. */
  openFilePicker: () => void;
  /** Currently-attached files (controlled). */
  files: File[];
}

type InputMessageSlot =
  | ReactNode
  | ((ctx: InputMessageSlotContext) => ReactNode);

/** Renders a slot. A function slot is called here, in its own component,
 *  so InputMessage's render never hands consumer code the context's
 *  ref-reading callbacks (openFilePicker) mid-render. */
function InputMessageSlotContent({
  slot,
  ctx,
}: {
  slot: InputMessageSlot;
  ctx: InputMessageSlotContext;
}) {
  return <>{typeof slot === "function" ? slot(ctx) : slot}</>;
}

interface InputMessageProps
  extends Omit<HTMLAttributes<HTMLDivElement>, "onChange"> {
  /** Step on the size ladder. Wins over the surrounding SizeProvider and
   *  propagates to the composer's rows and buttons. */
  size?: SizeVariant;
  /** Controlled textarea value. */
  value: string;
  /** Called with the new value on every textarea change. */
  onValueChange: (value: string) => void;
  /** Fired when the user submits (Enter or the send button) with the trimmed
   *  value and the attached files. Skipped when both are empty. */
  onSend?: (value: string, files: File[]) => void;
  /** Placeholder text shown when the value is empty. */
  placeholder?: string;
  /** Content rendered in the bottom-left action area. May be a render-fn
   *  that receives `{ openFilePicker, files }` to wire an attach button. */
  leftSlot?: InputMessageSlot;
  /** Content rendered in the bottom-right action area, before the built-in
   *  send button. Same render-fn form as `leftSlot`. */
  rightSlot?: InputMessageSlot;
  /** Disables the textarea and send button. */
  disabled?: boolean;
  /** Minimum visible rows before the textarea grows. */
  minRows?: number;
  /** Maximum visible rows before the textarea starts to scroll. */
  maxRows?: number;
  /** When false, clicking the surrounding container won't refocus the textarea. */
  clickToFocus?: boolean;
  /** Accessible label for the send button. */
  sendLabel?: string;
  /** Extra props forwarded to the underlying textarea. */
  textareaProps?: Omit<
    TextareaHTMLAttributes<HTMLTextAreaElement>,
    "value" | "onChange" | "onKeyDown" | "disabled" | "placeholder"
  >;
  /** Assistant response state. While `"streaming"` with `onStop` set, the
   *  send button is a Stop control; Enter still sends through `onSend`. */
  status?: "idle" | "streaming";
  /** Fired when the Stop control is pressed. */
  onStop?: () => void;
  /** Controlled list of attached files. When undefined, attachment behavior
   *  is disabled (no drag-drop, paste, or file input). */
  files?: File[];
  /** Called when files are added (drag-drop, paste or picker) or removed. */
  onFilesChange?: (files: File[]) => void;
  /** Accepted MIME types, comma-separated. Used by the picker, drop and paste. */
  accept?: string;
  /** Maximum number of files. */
  maxFiles?: number;
  /** Called with the files a drop, paste or pick left out: of a type `accept`
   *  refuses, or past `maxFiles`. */
  onFilesRejected?: (files: File[]) => void;
  /** Side length of the composer's attachment tiles in pixels. */
  filePreviewSize?: number;
  /** Makes each tile a button that opens its file, e.g. in a dialog. */
  onFileOpen?: (file: File) => void;
  /** While the consumer prepares the message: the draft stays and keeps the
   *  focus, read-only, and nothing can be sent, added or removed. */
  sending?: boolean;
  /** Content above the attachments, inside the composer, such as quotes to
   *  send along. */
  header?: ReactNode;
  /** Allows sending an empty value with no files, when `header` holds
   *  something to send. */
  sendable?: boolean;
}

// ─── File preview tile ────────────────────────────────────────────────────
// Composer-row tile: a FileThumbnail wrapped with enter/exit motion and a
// remove (×) button revealed on hover and on keyboard focus.
interface FilePreviewTileProps {
  file: File;
  /** How the remove button names the file: pasted images share one name, so
   *  the composer adds their place, "image.png, 2 of 3". */
  label: string;
  onRemove?: () => void;
  onOpen?: () => void;
  size: number;
  isTouch: boolean;
}

function FilePreviewTile({ file, label, onRemove, onOpen, size, isTouch }: FilePreviewTileProps) {
  const icons = useIcons();
  const XIcon = icons.x;
  const shape = useShape();
  // The tile sits 8px inside the composer's container edge. Derive its curve
  // from that actual inset instead of reusing the popup-specific 4px pair.
  const radius = nestedRadius(shape.containerRadius, 8);

  return (
    <motion.div
      // `layout` animates sibling tiles into the gap when one is removed.
      // Enter: spring.fast (0.08s) — the small-state-flip tier per motion-guidelines.md.
      // Exit: 0.06s linear — "exits should be slightly faster than enter",
      // matches CheckboxGroup's hover-bg pattern.
      layout
      initial={{ opacity: 0, scale: 0.9 }}
      animate={{ opacity: 1, scale: 1 }}
      exit={{ opacity: 0, scale: 0.9, transition: spring.fast.exit }}
      transition={spring.fast}
      // `cursor-default` opts out of the parent's `cursor-text` so hovering
      // a preview tile doesn't look like it'll land in the textarea.
      className="relative shrink-0 cursor-default group/tile"
    >
      {onOpen ? (
        <button
          type="button"
          onClick={onOpen}
          aria-label={`Open ${label}`}
          className="block cursor-zoom-in outline-none focus-visible:ring-1 focus-visible:ring-[color:var(--focus-ring,#6B97FF)]"
          style={{ borderRadius: radius }}
        >
          <FileThumbnail file={file} size={size} radius={radius} />
        </button>
      ) : (
        <FileThumbnail file={file} size={size} radius={radius} />
      )}
      {onRemove && (
        <Tooltip content="Remove" side="top">
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              onRemove();
            }}
            aria-label={`Remove ${label}`}
            // Force the light-mode palette (dark circle + white X) regardless
            // of theme — the close badge needs to read as a "delete affordance"
            // over arbitrary image/PDF content, so it sits at a fixed contrast
            // instead of flipping with the surrounding surface.
            className={cn(
              "absolute top-1 right-1 w-5 h-5 rounded-full bg-neutral-900 text-white transition-opacity duration-80 flex items-center justify-center cursor-pointer outline-none focus-visible:ring-1 focus-visible:ring-[color:var(--focus-ring,#6B97FF)]",
              // The 20px badge takes taps across a 32px square (a pseudo-element
              // that stays inside the tile's corner).
              "before:absolute before:-inset-1.5",
              // Hover devices reveal × on tile-hover; touch has no hover, so keep
              // it persistently visible there. While hidden it must not take
              // taps meant for the thumbnail.
              isTouch
                ? "opacity-100"
                : "pointer-events-none opacity-0 group-hover/tile:pointer-events-auto group-hover/tile:opacity-100 focus-visible:pointer-events-auto focus-visible:opacity-100"
            )}
          >
            <XIcon size={12} strokeWidth={2.5} />
          </button>
        </Tooltip>
      )}
    </motion.div>
  );
}



// ─── InputMessage ─────────────────────────────────────────────────────────

const InputMessage = forwardRef<HTMLDivElement, InputMessageProps>(
  (
    {
      size,
      value,
      onValueChange,
      onSend,
      placeholder = "Ask me anything…",
      leftSlot,
      rightSlot,
      disabled,
      minRows = 1,
      maxRows = 8,
      clickToFocus = true,
      sendLabel = "Send",
      textareaProps,
      status,
      onStop,
      files,
      onFilesChange,
      accept = DEFAULT_ACCEPT,
      maxFiles,
      onFilesRejected,
      filePreviewSize = 64,
      onFileOpen,
      sending = false,
      header,
      sendable = false,
      className,
      style,
      ...props
    },
    ref
  ) => {
    const shape = useShape();
    const compactStep = useSize(size).variant === "compact";
    const icons = useIcons();
    const ArrowUpIcon = icons["arrow-up"];
    const reduceMotion = useReducedMotion() ?? false;

    const textareaRef = useRef<HTMLTextAreaElement>(null);
    const [focusVisible, setFocusVisible] = useState(false);
    const [hovered, setHovered] = useState(false);
    const fileInputRef = useRef<HTMLInputElement>(null);
    const [dragOver, setDragOver] = useState(false);
    const isTouch = useTouchPrimary();
    const filesArr = useMemo(() => files ?? [], [files]);
    const supportsFiles = onFilesChange !== undefined && !sending;
    const [filesRegionRef, filesRegionHeight] = useRegionHeight();

    // Split out onFocus/onBlur so the rest-spread onto the textarea can't
    // clobber the composed handlers below, and className so it merges with
    // the field's own classes instead of replacing them.
    const { className: textareaClassName, ...textareaRest } = textareaProps ?? {};
    const restTextareaProps = omit(textareaRest, ["onFocus", "onBlur"]);

    const streaming = status === "streaming";

    // Parsed line-height, cached per textarea element — getComputedStyle on
    // every keystroke is needless work when the value only changes with font
    // or zoom changes.
    const lineHeightCache = useRef<{ el: HTMLTextAreaElement; value: number } | null>(null);

    const resizeTextarea = useCallback(() => {
      const el = textareaRef.current;
      if (!el) return;
      el.style.height = "auto";
      let cache = lineHeightCache.current;
      if (!cache || cache.el !== el) {
        const lineHeight = parseFloat(getComputedStyle(el).lineHeight);
        cache = { el, value: Number.isNaN(lineHeight) ? 20 : lineHeight };
        lineHeightCache.current = cache;
      }
      const min = cache.value * minRows;
      const max = cache.value * maxRows;
      const next = Math.min(Math.max(el.scrollHeight, min), max);
      el.style.height = `${next}px`;
      el.style.overflowY = el.scrollHeight > max ? "auto" : "hidden";
    }, [minRows, maxRows]);

    useIsoLayoutEffect(() => {
      resizeTextarea();
    }, [value, resizeTextarea]);

    // Re-measure when the textarea's width changes. The mount-time pass can
    // run while an ancestor is still laid out at (near-)zero width — the
    // wrapped placeholder then reads as many lines and pins the height at
    // maxRows until the next value change. Width-gated so the observer
    // doesn't loop on its own height writes.
    useEffect(() => {
      const el = textareaRef.current;
      if (!el || typeof ResizeObserver === "undefined") return;
      let lastWidth = el.offsetWidth;
      const ro = new ResizeObserver(() => {
        const width = el.offsetWidth;
        if (width === lastWidth) return;
        lastWidth = width;
        resizeTextarea();
      });
      ro.observe(el);
      return () => ro.disconnect();
    }, [resizeTextarea]);

    const trimmed = value.trim();
    const canSend = !disabled && !sending && (trimmed.length > 0 || filesArr.length > 0 || sendable);

    // Edge = the box-shadow's 1px ring, recoloured in place per state so the
    // stroke gains contrast without ever appearing to thicken (no second
    // border band layered beside it). The drop (`0 1px 1px`) is kept so the
    // composer holds its lift across states. Applied inline (not via a Tailwind
    // `shadow-*` utility, which mangles multi-layer arbitrary values) with the
    // precedence drag > focus > hover; when none are active, the className's
    // `shadow-surface-2` supplies the resting edge.
    const EDGE_DROP = "0 1px 1px -0.5px var(--shadow-color)";
    const edgeShadow = dragOver
      ? `0 0 0 1px var(--fluid-accent, #6B97FF), ${EDGE_DROP}`
      : focusVisible
      ? `0 0 0 1px color-mix(in oklab, var(--foreground) 20%, transparent), ${EDGE_DROP}`
      : hovered && clickToFocus && !disabled
        ? `0 0 0 1px var(--border), ${EDGE_DROP}`
        : undefined;

    const handleSend = useCallback(() => {
      if (!canSend) return;
      onSend?.(trimmed, filesArr);
    }, [canSend, onSend, trimmed, filesArr]);

    const handleStop = useCallback(() => onStop?.(), [onStop]);

    const stopMode = streaming && onStop !== undefined;

    const handleKeyDown = useCallback(
      (e: ReactKeyboardEvent<HTMLTextAreaElement>) => {
        if (e.nativeEvent.isComposing) return;
        if (e.key === "Enter" && !e.shiftKey) {
          e.preventDefault();
          handleSend();
        }
      },
      [handleSend]
    );

    const handleContainerMouseDown = useCallback(
      (e: React.MouseEvent<HTMLDivElement>) => {
        if (!clickToFocus || disabled) return;
        const target = e.target as HTMLElement;
        if (target === textareaRef.current) return;
        if (
          target.closest(
            'button, a, input, select, textarea, [contenteditable], [role="button"]'
          )
        ) {
          return;
        }
        e.preventDefault();
        textareaRef.current?.focus();
      },
      [clickToFocus, disabled]
    );

    // ── File helpers ──────────────────────────────────────────────────
    const acceptTokens = useMemo(
      () => accept.split(",").map((s) => s.trim()).filter(Boolean),
      [accept]
    );

    const matchesAccept = useCallback(
      (file: File) =>
        acceptTokens.some((token) => {
          if (token.endsWith("/*")) return file.type.startsWith(token.slice(0, -1));
          if (token.startsWith(".")) return file.name.toLowerCase().endsWith(token.toLowerCase());
          return file.type === token;
        }),
      [acceptTokens]
    );

    const addFiles = useCallback(
      (incoming: File[]) => {
        if (!onFilesChange) return;
        // Identity key for dedup: name + size + lastModified is unique enough
        // to catch "user dropped the same file twice" without false positives
        // on legitimately distinct files (different bytes ⇒ different size).
        const fingerprint = (f: File) => `${f.name}-${f.size}-${f.lastModified}`;
        const existing = new Set(filesArr.map(fingerprint));
        const accepted: File[] = [];
        const rejected: File[] = [];
        for (const f of incoming) {
          if (!matchesAccept(f)) {
            rejected.push(f);
            continue;
          }
          const fp = fingerprint(f);
          if (existing.has(fp)) continue;
          existing.add(fp);
          accepted.push(f);
        }
        const room = maxFiles != null ? Math.max(0, maxFiles - filesArr.length) : accepted.length;
        rejected.push(...accepted.splice(room));
        if (accepted.length) onFilesChange([...filesArr, ...accepted]);
        // Last, so a consumer that clears its notice on change still shows it.
        if (rejected.length) onFilesRejected?.(rejected);
      },
      [onFilesChange, onFilesRejected, filesArr, matchesAccept, maxFiles]
    );

    const removeFile = useCallback(
      (idx: number) => {
        if (!onFilesChange) return;
        onFilesChange(filesArr.filter((_, i) => i !== idx));
        // The tile's × goes with it; the caret takes the focus back.
        textareaRef.current?.focus();
      },
      [onFilesChange, filesArr]
    );

    const openFilePicker = useCallback(() => fileInputRef.current?.click(), []);

    // ── Slot rendering ────────────────────────────────────────────────
    const slotCtx = useMemo<InputMessageSlotContext>(
      () => ({ openFilePicker, files: filesArr }),
      [openFilePicker, filesArr]
    );
    const leftContent = <InputMessageSlotContent slot={leftSlot} ctx={slotCtx} />;
    const rightContent = <InputMessageSlotContent slot={rightSlot} ctx={slotCtx} />;

    // ── Drag-and-drop ────────────────────────────────────────────────
    const handleDragOver = useCallback(
      (e: ReactDragEvent<HTMLDivElement>) => {
        if (!supportsFiles || disabled) return;
        // Only treat as a file drag — text/HTML drags shouldn't trigger.
        if (!Array.from(e.dataTransfer.types).includes("Files")) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = "copy";
        setDragOver(true);
      },
      [supportsFiles, disabled]
    );

    const handleDragLeave = useCallback(
      (e: ReactDragEvent<HTMLDivElement>) => {
        const wrapper = e.currentTarget;
        const next = e.relatedTarget as Node | null;
        if (next && wrapper.contains(next)) return;
        setDragOver(false);
      },
      []
    );

    const handleDrop = useCallback(
      (e: ReactDragEvent<HTMLDivElement>) => {
        e.preventDefault();
        setDragOver(false);
        if (!supportsFiles || disabled) return;
        addFiles(Array.from(e.dataTransfer.files));
      },
      [supportsFiles, disabled, addFiles]
    );

    const handleFileInputChange = useCallback(
      (e: ChangeEvent<HTMLInputElement>) => {
        if (!e.target.files) return;
        addFiles(Array.from(e.target.files));
        e.target.value = ""; // Allow re-selecting the same file.
        textareaRef.current?.focus();
      },
      [addFiles]
    );

    // A pasted screenshot, image or copied file attaches, even when the
    // clipboard also names it in text or wraps it in HTML ("Copy image").
    // Rich text stays text: cells copied from a spreadsheet bring an image of
    // themselves along with both their text and their HTML.
    const handlePaste = useCallback(
      (e: ReactClipboardEvent<HTMLTextAreaElement>) => {
        if (!supportsFiles || disabled) return;
        const data = e.clipboardData;
        if (data.types.includes("text/html") && data.getData("text/plain")) return;
        const pasted = Array.from(e.clipboardData.files);
        if (!pasted.some(matchesAccept)) return;
        e.preventDefault();
        addFiles(pasted);
      },
      [supportsFiles, disabled, addFiles, matchesAccept]
    );

    const composer = (
      <div
        ref={ref}
        onMouseDown={handleContainerMouseDown}
        onDragOver={handleDragOver}
        onDragLeave={handleDragLeave}
        onDrop={handleDrop}
        className={cn(
          // The edge is the box-shadow's hairline ring (from surface-2), not a
          // border. State changes recolor that same 1px ring in place rather
          // than layering a second colored border beside it — so hover / focus
          // bump *contrast* without ever appearing to thicken the stroke.
          "flex flex-col gap-1 p-2 transition-[box-shadow,color] duration-80",
          surfaceClasses(2, 2),
          shape.container,
          clickToFocus && !disabled && "cursor-text",
          disabled && "opacity-50 pointer-events-none",
          className
        )}
        style={edgeShadow ? { boxShadow: edgeShadow, ...style } : style}
        onMouseEnter={() => setHovered(true)}
        onMouseLeave={() => setHovered(false)}
        {...props}
      >
        <SurfaceProvider value={2}>
          {supportsFiles && (
            <input
              ref={fileInputRef}
              type="file"
              accept={accept}
              multiple={maxFiles == null || maxFiles > 1}
              className="hidden"
              onChange={handleFileInputChange}
              aria-hidden="true"
              tabIndex={-1}
            />
          )}

          {header}

          {/* Attached files preview row — sits above the textarea.
              The outer motion.div animates the row's height (collapsing the
              whole component height) when files appear / disappear.
              The inner `mode="popLayout"` AnimatePresence pulls a removing
              tile out of layout flow so siblings can slide into the gap
              without fighting its exit anim. Keys are purely file-identity
              (no index) so removing the first file doesn't re-key — and
              remount — every surviving sibling. */}
          <AnimatePresence initial={false}>
            {filesArr.length > 0 && (
              <motion.div
                key="preview-row"
                initial={{ height: 0, opacity: 0 }}
                animate={{ height: filesRegionHeight ?? 0, opacity: 1 }}
                exit={{ height: 0, opacity: 0 }}
                transition={{ ...spring.moderate, bounce: 0 }}
                className="overflow-hidden"
              >
                <div ref={filesRegionRef} className="flex flex-wrap gap-2 pb-1">
                  <AnimatePresence initial={false} mode="popLayout">
                    {filesArr.map((file, i) => (
                      <FilePreviewTile
                        key={`${file.name}-${file.size}-${file.lastModified}`}
                        file={file}
                        label={filesArr.length > 1 ? `${file.name}, ${i + 1} of ${filesArr.length}` : file.name}
                        onRemove={sending ? undefined : () => removeFile(i)}
                        onOpen={onFileOpen && (() => onFileOpen(file))}
                        size={filePreviewSize}
                        isTouch={isTouch}
                      />
                    ))}
                  </AnimatePresence>
                </div>
              </motion.div>
            )}
          </AnimatePresence>

          <div className="relative">
            <textarea
              ref={textareaRef}
              value={value}
              onChange={(e) => onValueChange(e.target.value)}
              onKeyDown={handleKeyDown}
              onPaste={handlePaste}
              // Compose the consumer's textareaProps handlers with the internal
              // focus-visible tracking (the spread below would otherwise
              // overwrite these).
              onFocus={(e) => {
                if (e.target.matches(":focus-visible")) setFocusVisible(true);
                textareaProps?.onFocus?.(e);
              }}
              onBlur={(e) => {
                setFocusVisible(false);
                textareaProps?.onBlur?.(e);
              }}
              placeholder={dragOver && supportsFiles ? "Drop files here to add to chat" : placeholder}
              disabled={disabled}
              readOnly={sending}
              aria-busy={sending || undefined}
              rows={minRows}
              aria-label={textareaProps?.["aria-label"] ?? "Message"}
              className={cn(
                "w-full resize-none rounded-none bg-transparent outline-none",
                "text-foreground placeholder:text-muted-foreground",
                compactStep
                  ? "text-[length:var(--fs-subtitle-compact,13px)] leading-[var(--lh-subtitle-compact,18px)] px-1.5 py-1.5"
                  : "text-[length:var(--fs-subtitle,14px)] leading-[var(--lh-subtitle,20px)] px-2 py-2",
                fieldTouchClass,
                textareaClassName
              )}
              style={{ fontVariationSettings: fontWeights.normal }}
              {...restTextareaProps}
            />
          </div>
          <div
            className={cn(
              "flex items-center justify-between",
              // The footer's controls sit one notch below the composer's step:
              // slot content is consumer-authored (usually sm/icon-sm pinned
              // Buttons), so the compact step scales any button in the row —
              // send button included — down to 24px via a scoped override.
              compactStep
                ? "gap-1.5 [&_button]:h-6 [&_button.w-7]:w-6 [&_button]:text-[length:var(--fs-caption-compact,11px)] [&_button]:leading-[var(--lh-caption-compact,14px)]"
                : "gap-2"
            )}
          >
            <div className="flex items-center gap-1.5 min-w-0">{leftContent}</div>
            <div className="flex items-center gap-1.5 shrink-0">
              {rightContent}
              <Button
                type="button"
                variant="primary"
                size="icon-sm"
                onClick={stopMode ? handleStop : handleSend}
                disabled={stopMode ? disabled : !canSend}
                loading={sending && !stopMode}
                aria-label={stopMode ? "Stop" : sendLabel}
              >
                <AnimatePresence mode="wait" initial={false}>
                  <motion.span
                    key={stopMode ? "stop" : "arrow"}
                    initial={
                      reduceMotion
                        ? { opacity: 0 }
                        : { opacity: 0, scale: 0.6 }
                    }
                    animate={{ opacity: 1, scale: 1 }}
                    exit={
                      reduceMotion
                        ? { opacity: 0 }
                        : { opacity: 0, scale: 0.6, transition: spring.fast.exit }
                    }
                    transition={spring.fast}
                    className="flex items-center justify-center leading-none"
                  >
                    {stopMode ? (
                      <span className="h-3 w-3 rounded-[3px] bg-current" />
                    ) : (
                      // Override icon-sm's small 14px svg — the send glyph reads
                      // better a touch larger. `size` matches the attribute to
                      // the CSS so the svg box stays centered.
                      <ArrowUpIcon
                        size={compactStep ? 15 : 19}
                        className={cn(
                          "block",
                          compactStep
                            ? "!h-[15px] !w-[15px]"
                            : "!h-[19px] !w-[19px]"
                        )}
                      />
                    )}
                  </motion.span>
                </AnimatePresence>
              </Button>
            </div>
          </div>
        </SurfaceProvider>
      </div>
    );

    // A size prop pins the whole composer — inner buttons and rows included —
    // to one ladder step (matches InputGroup).
    return size ? <SizeProvider size={size}>{composer}</SizeProvider> : composer;
  }
);

InputMessage.displayName = "InputMessage";

export { InputMessage };
export type { InputMessageProps, InputMessageSlotContext };
export default InputMessage;
