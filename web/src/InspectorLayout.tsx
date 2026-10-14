import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { CSSProperties, ReactNode } from "react";

const defaultWidth = 340;
const minWidth = 300;
const mapMinWidth = 320;
const splitterWidth = 24;
const storageKey = "arena-inspector-width";

export function InspectorLayout({
  children,
  hasInspector,
}: {
  children: [ReactNode, ReactNode];
  hasInspector: boolean;
}) {
  const layout = useRef<HTMLDivElement>(null);
  const drag = useRef<{
    pointerId: number;
    startX: number;
    startWidth: number;
  } | null>(null);
  const [availableWidth, setAvailableWidth] = useState(0);
  const [preferredWidth, setPreferredWidth] = useState(() => {
    try {
      const saved = Number(localStorage.getItem(storageKey));
      if (Number.isFinite(saved) && saved >= minWidth) return saved;
    } catch {
      // Resizing still works when browser storage is unavailable.
    }
    return defaultWidth;
  });
  const [dragging, setDragging] = useState(false);
  const maxWidth = Math.max(
    minWidth,
    availableWidth - mapMinWidth - splitterWidth,
  );
  const width = Math.min(maxWidth, Math.max(minWidth, preferredWidth));
  const resize = (next: number) =>
    setPreferredWidth(Math.round(Math.min(maxWidth, Math.max(minWidth, next))));

  useLayoutEffect(() => {
    const element = layout.current!;
    const observer = new ResizeObserver(([entry]) =>
      setAvailableWidth(entry.contentRect.width),
    );
    setAvailableWidth(element.getBoundingClientRect().width);
    observer.observe(element);
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    try {
      localStorage.setItem(storageKey, String(preferredWidth));
    } catch {
      // Keep the current preference in memory if storage is unavailable.
    }
  }, [preferredWidth]);

  useEffect(() => {
    if (!dragging || !hasInspector) return;
    document.documentElement.classList.add("inspector-resizing");
    return () =>
      document.documentElement.classList.remove("inspector-resizing");
  }, [dragging, hasInspector]);

  return (
    <div
      ref={layout}
      className={"arena-layout " + (hasInspector ? "has-inspector" : "")}
      style={{ "--inspector-width": `${width}px` } as CSSProperties}
    >
      {children[0]}
      {hasInspector && (
        <div
          className="inspector-splitter"
          role="separator"
          aria-label="Resize node details"
          aria-orientation="vertical"
          aria-controls="node-inspector"
          aria-valuemin={minWidth}
          aria-valuemax={Math.round(maxWidth)}
          aria-valuenow={width}
          aria-valuetext={`${width} pixels wide`}
          tabIndex={0}
          title="Drag to resize details. Use Left/Right arrows, or double-click to reset."
          onPointerDown={(event) => {
            if (event.button !== 0 || !event.isPrimary) return;
            event.preventDefault();
            event.currentTarget.focus();
            event.currentTarget.setPointerCapture(event.pointerId);
            drag.current = {
              pointerId: event.pointerId,
              startX: event.clientX,
              startWidth: width,
            };
            setDragging(true);
          }}
          onPointerMove={(event) => {
            const active = drag.current;
            if (!active || active.pointerId !== event.pointerId) return;
            resize(active.startWidth + active.startX - event.clientX);
          }}
          onPointerUp={(event) => {
            if (drag.current?.pointerId !== event.pointerId) return;
            event.currentTarget.releasePointerCapture(event.pointerId);
            drag.current = null;
            setDragging(false);
          }}
          onPointerCancel={() => {
            drag.current = null;
            setDragging(false);
          }}
          onLostPointerCapture={() => {
            drag.current = null;
            setDragging(false);
          }}
          onDoubleClick={() => resize(defaultWidth)}
          onKeyDown={(event) => {
            const step = event.shiftKey ? 50 : 10;
            switch (event.key) {
              case "ArrowLeft":
                resize(width + step);
                break;
              case "ArrowRight":
                resize(width - step);
                break;
              case "Home":
                resize(minWidth);
                break;
              case "End":
                resize(maxWidth);
                break;
              default:
                return;
            }
            event.preventDefault();
          }}
        />
      )}
      {children[1]}
    </div>
  );
}
