"use client";

import {
  forwardRef,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
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
import { useShape } from "@/lib/shape-context";
import { SizeProvider, useSize, fieldTouchClass, type SizeVariant } from "@/lib/size-context";
import { useIcons } from "@/lib/icon-context";
import { surfaceClasses } from "@/lib/surface-classes";
import { SurfaceProvider } from "@/lib/surface-context";
import { Button } from "@/components/ui/button";

const useIsoLayoutEffect =
  typeof window !== "undefined" ? useLayoutEffect : useEffect;

interface InputMessageProps
  extends Omit<HTMLAttributes<HTMLDivElement>, "onChange"> {
  /** Step on the size ladder. Wins over the surrounding SizeProvider and
   *  propagates to the composer's rows and buttons. */
  size?: SizeVariant;
  /** Controlled textarea value. */
  value: string;
  /** Called with the new value on every textarea change. */
  onValueChange: (value: string) => void;
  /** Fired when the user submits (Enter or the send button) with the trimmed value. */
  onSend?: (value: string) => void;
  /** Placeholder text shown when the value is empty. */
  placeholder?: string;
  /** Content rendered in the bottom-left action area. */
  leftSlot?: ReactNode;
  /** Content rendered in the bottom-right action area, before the built-in
   *  send button. */
  rightSlot?: ReactNode;
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
    const canSend = !disabled && trimmed.length > 0;

    // Edge = the box-shadow's 1px ring, recoloured in place per state so the
    // stroke gains contrast without ever appearing to thicken (no second
    // border band layered beside it). The drop (`0 1px 1px`) is kept so the
    // composer holds its lift across states. Applied inline (not via a Tailwind
    // `shadow-*` utility, which mangles multi-layer arbitrary values) with the
    // precedence focus > hover; when neither is active, the className's
    // `shadow-surface-2` supplies the resting edge.
    const EDGE_DROP = "0 1px 1px -0.5px var(--shadow-color)";
    const edgeShadow = focusVisible
      ? `0 0 0 1px color-mix(in oklab, var(--foreground) 20%, transparent), ${EDGE_DROP}`
      : hovered && clickToFocus && !disabled
        ? `0 0 0 1px var(--border), ${EDGE_DROP}`
        : undefined;

    const handleSend = useCallback(() => {
      if (!canSend) return;
      onSend?.(trimmed);
    }, [canSend, onSend, trimmed]);

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

    const composer = (
      <div
        ref={ref}
        onMouseDown={handleContainerMouseDown}
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
          <div className="relative">
            <textarea
              ref={textareaRef}
              value={value}
              onChange={(e) => onValueChange(e.target.value)}
              onKeyDown={handleKeyDown}
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
              placeholder={placeholder}
              disabled={disabled}
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
            <div className="flex items-center gap-1.5 min-w-0">{leftSlot}</div>
            <div className="flex items-center gap-1.5 shrink-0">
              {rightSlot}
              <Button
                type="button"
                variant="primary"
                size="icon-sm"
                onClick={stopMode ? handleStop : handleSend}
                disabled={stopMode ? disabled : !canSend}
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
export type { InputMessageProps };
export default InputMessage;
