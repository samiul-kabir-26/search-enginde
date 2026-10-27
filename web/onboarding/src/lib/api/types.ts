import type { components } from "./control";

type S = components["schemas"];

export type FieldError = S["FieldError"];
export type Problem = S["Problem"];
export type SessionResponse = S["SessionResponse"];
export type OnboardingStep = S["OnboardingStep"];
export type OnboardingState = S["OnboardingState"];
export type StepResult = S["StepResult"];
export type EngineExplainer = S["EngineExplainer"];
export type EngineStage = EngineExplainer["stages"][number];
export type DraftView = S["DraftView"];
export type Model = S["Model"];
export type ModelInput = S["ModelInput"];
export type ModelValidation = S["ModelValidation"];
export type PlatformModel = S["PlatformModel"];
export type Connection = S["Connection"];
export type ConnectionInput = S["ConnectionInput"];
export type ConnectionTestResult = S["ConnectionTestResult"];
export type VectorSpec = S["VectorSpec"];
export type IndexJob = S["IndexJob"];
export type PresignedUpload = S["PresignedUpload"];
export type MappingPreviewResponse = S["MappingPreviewResponse"];
export type PolicyTestResponse = S["PolicyTestResponse"];
export type PlatformSettings = S["PlatformSettings"];
export type Plan = S["Plan"];
export type SigningKey = S["SigningKey"];
export type Tenant = S["Tenant"];
export type User = S["User"];

/** search-api's SearchResponse as the playground returns it (search.yaml), always with a trace. */
export type SearchResponse = {
  request_id?: string;
  total?: number;
  degraded?: boolean;
  results: {
    product_id: string;
    score: number;
    sponsored: boolean;
    position: number;
    product?: { title: string; brand?: string; price_minor: number; currency: string; image_url?: string; vendor_id: string };
  }[];
  trace?: { stages: StageTrace[] };
};

export type StageTrace = {
  stage: string;
  provider: string;
  version?: string;
  latency_ms: number;
  in_count: number;
  out_count: number;
  fallback: boolean;
  fallback_reason?: string;
  error?: string;
  candidates?: { product_id: string; scores: Record<string, number> }[];
};

/** The steps after sign-up, in wizard order (docs/ONBOARDING.md §2). */
export const WIZARD_STEPS = [
  "engine_tour",
  "catalog",
  "retrieval",
  "policies",
  "ranking",
  "personalization",
  "index_build",
  "playground",
  "go_live",
] as const satisfies readonly OnboardingStep[];

export type WizardStep = (typeof WIZARD_STEPS)[number];

export const STEP_TITLES: Record<WizardStep, string> = {
  engine_tour: "Engine tour",
  catalog: "Catalog",
  retrieval: "Retrieval",
  policies: "Policies",
  ranking: "Ranking",
  personalization: "Personalization & ads",
  index_build: "Index build",
  playground: "Playground",
  go_live: "Go live",
};

export function isWizardStep(s: string): s is WizardStep {
  return (WIZARD_STEPS as readonly string[]).includes(s);
}

/** The outcome of a form action: field errors keyed by JSON Pointer, plus a form-level message. */
export type FormState = {
  ok: boolean;
  message?: string;
  errors?: FieldError[];
  warnings?: FieldError[];
  /** The submitted text values, so a failed submit keeps what the user typed. */
  values?: Record<string, string>;
};

export const initialFormState: FormState = { ok: false };
