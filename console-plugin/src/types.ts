import type { K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';

// GroupVersionKind for the Chronos ChangeEvent CRD (chronos.ocp.run/v1alpha1).
export const CHANGE_EVENT_GVK = {
  group: 'chronos.ocp.run',
  version: 'v1alpha1',
  kind: 'ChangeEvent',
};

export type AttributionConfidence = 'verified' | 'partial' | 'unattributed';
export type RiskLevel = 'low' | 'medium' | 'high' | 'critical';
export type ChangeVerb = 'create' | 'update' | 'patch' | 'delete';

export interface Actor {
  username?: string;
  uid?: string;
  groups?: string[];
  sourceIP?: string;
  userAgent?: string;
  confidence: AttributionConfidence;
  shared?: boolean;
}

export interface TargetObjectReference {
  apiVersion: string;
  kind: string;
  namespace?: string;
  name: string;
  uid?: string;
  resourceVersion?: string;
}

export interface ChangeEvent extends K8sResourceCommon {
  spec: {
    observedAt: string;
    verb: ChangeVerb;
    target: TargetObjectReference;
    actor: Actor;
    source: string;
    riskLevel?: RiskLevel;
    summary?: string;
    changedFields?: string[];
    beforeSnapshot?: string;
    afterSnapshot?: string;
    redacted?: boolean;
  };
}

export const RESOURCE_SNAPSHOT_GVK = {
  group: 'chronos.ocp.run',
  version: 'v1alpha1',
  kind: 'ResourceSnapshot',
};

export interface RedactionEntry {
  fieldPath: string;
  reason: string;
  hash?: string;
}

export interface ResourceSnapshot extends K8sResourceCommon {
  spec: {
    target: TargetObjectReference;
    capturedAt: string;
    // The redacted object manifest, as a decoded object.
    content?: Record<string, unknown>;
    redactions?: RedactionEntry[];
    redacted: boolean;
    revertable: boolean;
  };
}

export type ViewMode = 'timeline' | 'calendar' | 'list';
