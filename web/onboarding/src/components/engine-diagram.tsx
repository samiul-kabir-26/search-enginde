"use client";

import { useId, useState, type KeyboardEvent } from "react";

import type { EngineStage } from "@/lib/api/types";

import { StagePanel } from "./stage-panel";
import { Badge, cx } from "./ui";

/**
 * The six-stage engine explainer (docs/ONBOARDING.md §3). Desktop: an SVG flow with dashed
 * injection nodes. Under 768 px: a vertical list with the injection node as a chip row under
 * each stage. Every stage is a button with aria-expanded controlling the StagePanel, and an
 * ordered list gives screen readers the same content as text.
 */
export function EngineDiagram({ stages, linkSteps = true }: { stages: EngineStage[]; linkSteps?: boolean }) {
  const [open, setOpen] = useState<string | null>(null);
  const panelId = useId();
  const selected = stages.find((s) => s.id === open) ?? null;
  const toggle = (id: string) => setOpen((cur) => (cur === id ? null : id));

  return (
    <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_20rem]">
      <div>
        <ol className="sr-only">
          {stages.map((s) => (
            <li key={s.id}>
              Stage {s.number}, {s.title}: {s.summary} Plugs in: {s.injection}
              {s.badge ? ` (${s.badge})` : ""}.
            </li>
          ))}
          <li>Results feed a learning loop: clicks, carts and orders retrain quick scoring and precise ranking.</li>
        </ol>

        <MobileFlow stages={stages} open={open} panelId={panelId} onToggle={toggle} />
        <DesktopFlow stages={stages} open={open} panelId={panelId} onToggle={toggle} />
      </div>

      <div id={panelId} aria-live="polite">
        {selected ? (
          <StagePanel stage={selected} onClose={() => setOpen(null)} linkStep={linkSteps} />
        ) : (
          <p className="rounded-lg border border-dashed border-zinc-300 p-4 text-sm text-zinc-500 dark:border-zinc-700">
            Select a stage to see what it does, what you can configure and your current choice.
          </p>
        )}
      </div>
    </div>
  );
}

type FlowProps = { stages: EngineStage[]; open: string | null; panelId: string; onToggle: (id: string) => void };

function MobileFlow({ stages, open, panelId, onToggle }: FlowProps) {
  return (
    <ol className="flex flex-col items-stretch gap-2 md:hidden" aria-label="Search pipeline stages">
      <li className="rounded-md bg-zinc-100 px-3 py-2 text-center text-sm dark:bg-zinc-800">Shopper query: text, image or similar</li>
      {stages.map((s) => (
        <li key={s.id} className="flex flex-col gap-1">
          <span aria-hidden className="text-center text-zinc-400">
            ↓
          </span>
          <button
            type="button"
            aria-expanded={open === s.id}
            aria-controls={panelId}
            onClick={() => onToggle(s.id)}
            className={cx(
              "rounded-lg border-2 px-3 py-2 text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-indigo-500",
              open === s.id ? "border-indigo-600 bg-indigo-50 dark:bg-indigo-950" : "border-zinc-300 bg-white dark:border-zinc-700 dark:bg-zinc-900",
            )}
          >
            <span className="block text-sm font-semibold">
              {s.number}. {s.title}
            </span>
            <span className="block text-xs text-zinc-600 dark:text-zinc-400">{s.summary}</span>
          </button>
          <div className="flex flex-wrap gap-1 pl-2">
            <span className="inline-flex items-center gap-1 rounded-full border border-dashed border-zinc-400 px-2 py-0.5 text-xs">
              {s.injection}
              {s.badge && <Badge tone={s.badge === "Custom" ? "indigo" : "zinc"}>{s.badge}</Badge>}
            </span>
          </div>
        </li>
      ))}
      <li className="flex flex-col gap-1">
        <span aria-hidden className="text-center text-zinc-400">
          ↓
        </span>
        <span className="rounded-md bg-emerald-100 px-3 py-2 text-center text-sm dark:bg-emerald-900">Results → learning loop</span>
      </li>
    </ol>
  );
}

const W = 150;
const GAP = 22;
const X0 = 130;
const STAGE_Y = 118;
const STAGE_H = 92;
const INJ_Y = 16;
const INJ_H = 50;

function DesktopFlow({ stages, open, panelId, onToggle }: FlowProps) {
  const x = (i: number) => X0 + i * (W + GAP);
  const end = x(stages.length);
  const width = end + 120;
  const onKey = (e: KeyboardEvent, id: string) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onToggle(id);
    }
  };
  const idx = (id: string) => stages.findIndex((s) => s.id === id);
  const loop = [idx("prerank"), idx("rank")].filter((i) => i >= 0);

  return (
    <svg
      viewBox={`0 0 ${width} 300`}
      className="hidden w-full md:block"
      role="group"
      aria-label="Search pipeline: select a stage to learn more"
    >
      <defs>
        <marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
          <path d="M 0 0 L 10 5 L 0 10 z" className="fill-zinc-400" />
        </marker>
      </defs>

      <g aria-hidden>
        <rect x={8} y={STAGE_Y + 16} width={100} height={60} rx={10} className="fill-zinc-100 dark:fill-zinc-800" />
        <text x={58} y={STAGE_Y + 42} textAnchor="middle" className="fill-zinc-700 text-[12px] dark:fill-zinc-200">
          Shopper
        </text>
        <text x={58} y={STAGE_Y + 58} textAnchor="middle" className="fill-zinc-700 text-[12px] dark:fill-zinc-200">
          query
        </text>
        <line x1={108} y1={STAGE_Y + 46} x2={X0 - 4} y2={STAGE_Y + 46} className="stroke-zinc-400" strokeWidth={2} markerEnd="url(#arrow)" />
        {stages.slice(0, -1).map((s, i) => (
          <line key={`a-${s.id}`} x1={x(i) + W} y1={STAGE_Y + 46} x2={x(i + 1) - 4} y2={STAGE_Y + 46} className="stroke-zinc-400" strokeWidth={2} markerEnd="url(#arrow)" />
        ))}
        <line x1={x(stages.length - 1) + W} y1={STAGE_Y + 46} x2={end + 6} y2={STAGE_Y + 46} className="stroke-zinc-400" strokeWidth={2} markerEnd="url(#arrow)" />
        <rect x={end + 10} y={STAGE_Y + 16} width={100} height={60} rx={10} className="fill-emerald-100 dark:fill-emerald-900" />
        <text x={end + 60} y={STAGE_Y + 50} textAnchor="middle" className="fill-emerald-900 text-[12px] dark:fill-emerald-100">
          Results
        </text>
        {/* Learning loop: clicks, carts and orders retrain stages 4 and 5. */}
        <path
          d={`M ${end + 60} ${STAGE_Y + 76} V 270 H ${x(loop[0] ?? 3) + W / 2} V ${STAGE_Y + STAGE_H + 4}`}
          fill="none"
          className="stroke-emerald-500"
          strokeWidth={2}
          strokeDasharray="6 5"
          markerEnd="url(#arrow)"
        />
        {loop[1] !== undefined && (
          <line x1={x(loop[1]) + W / 2} y1={270} x2={x(loop[1]) + W / 2} y2={STAGE_Y + STAGE_H + 4} className="stroke-emerald-500" strokeWidth={2} strokeDasharray="6 5" markerEnd="url(#arrow)" />
        )}
        <text x={(x(loop[0] ?? 3) + end) / 2 + 40} y={290} textAnchor="middle" className="fill-emerald-700 text-[11px] dark:fill-emerald-300">
          clicks, carts, orders → learning loop
        </text>
      </g>

      {stages.map((s, i) => {
        const active = open === s.id;
        return (
          <g key={s.id}>
            {/* Injection node: dashed, with its Default/Custom badge. */}
            <g aria-hidden>
              <rect x={x(i)} y={INJ_Y} width={W} height={INJ_H} rx={8} strokeDasharray="5 4" strokeWidth={1.5} className="fill-transparent stroke-zinc-400" />
              <text x={x(i) + W / 2} y={INJ_Y + 21} textAnchor="middle" className="fill-zinc-700 text-[11px] dark:fill-zinc-300">
                {s.injection}
              </text>
              {s.badge && (
                <text
                  x={x(i) + W / 2}
                  y={INJ_Y + 38}
                  textAnchor="middle"
                  className={cx("text-[10px] font-semibold", s.badge === "Custom" ? "fill-indigo-600 dark:fill-indigo-300" : "fill-zinc-500")}
                >
                  {s.badge}
                </text>
              )}
              <line x1={x(i) + W / 2} y1={INJ_Y + INJ_H} x2={x(i) + W / 2} y2={STAGE_Y - 4} className="stroke-zinc-400" strokeDasharray="4 4" markerEnd="url(#arrow)" />
            </g>
            <g
              role="button"
              tabIndex={0}
              aria-expanded={active}
              aria-controls={panelId}
              aria-label={`${s.number}. ${s.title}`}
              onClick={() => onToggle(s.id)}
              onKeyDown={(e) => onKey(e, s.id)}
              className="cursor-pointer outline-none [&:focus-visible>rect]:stroke-indigo-500 [&:focus-visible>rect]:stroke-[4]"
            >
              <rect
                x={x(i)}
                y={STAGE_Y}
                width={W}
                height={STAGE_H}
                rx={12}
                strokeWidth={2}
                className={active ? "fill-indigo-50 stroke-indigo-600 dark:fill-indigo-950" : "fill-white stroke-zinc-300 dark:fill-zinc-900 dark:stroke-zinc-600"}
              />
              <text x={x(i) + 12} y={STAGE_Y + 26} className="fill-indigo-700 text-[12px] font-semibold dark:fill-indigo-300">
                {s.number}
              </text>
              {wrap(s.title, 16).map((line, j) => (
                <text key={j} x={x(i) + 12} y={STAGE_Y + 46 + j * 16} className="fill-zinc-900 text-[13px] font-semibold dark:fill-zinc-100">
                  {line}
                </text>
              ))}
            </g>
          </g>
        );
      })}
    </svg>
  );
}

/** Greedy word wrap for SVG text. */
function wrap(text: string, max: number): string[] {
  const lines: string[] = [];
  let cur = "";
  for (const word of text.split(" ")) {
    if (cur && (cur + " " + word).length > max) {
      lines.push(cur);
      cur = word;
    } else {
      cur = cur ? cur + " " + word : word;
    }
  }
  if (cur) lines.push(cur);
  return lines;
}
