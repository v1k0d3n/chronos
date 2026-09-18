import * as React from 'react';
import {
  Alert,
  Button,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
} from '@patternfly/react-core';
import type {
  K8sModel,
  K8sResourceCommon} from '@openshift-console/dynamic-plugin-sdk';
import {
  k8sCreate,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
import type { TargetObjectReference } from '../types';
import type { Version } from '../objectHistory';

const RevertOperationModel: K8sModel = {
  apiGroup: 'chronos.ocp.run',
  apiVersion: 'v1alpha1',
  kind: 'RevertOperation',
  plural: 'revertoperations',
  namespaced: true,
  abbr: 'RO',
  label: 'RevertOperation',
  labelPlural: 'RevertOperations',
};

type RevertOperation = K8sResourceCommon & {
  spec?: Record<string, unknown>;
  status?: {
    phase?: 'Pending' | 'Running' | 'Succeeded' | 'Failed' | 'Skipped';
    message?: string;
    manualStepsRequired?: string[];
  };
};

// Creating a RevertOperation only files the request. Chronos then decides
// whether to carry it out — it refuses, for one, when the person asking could
// not have made the change themselves — so the modal waits for that verdict
// instead of reporting success on create.
const RevertOutcome: React.FC<{ name: string; namespace?: string }> = ({
  name,
  namespace,
}) => {
  // The SDK types the error slot as `any`; name it `unknown` at the boundary.
  const watched: [RevertOperation | undefined, boolean, unknown] = useK8sWatchResource<RevertOperation>({
    groupVersionKind: {
      group: 'chronos.ocp.run',
      version: 'v1alpha1',
      kind: 'RevertOperation',
    },
    name,
    namespace,
  });
  const [ro, loaded, loadError] = watched;

  const phase = ro?.status?.phase;
  const message = ro?.status?.message;
  const steps = ro?.status?.manualStepsRequired ?? [];

  if (loadError) {
    return (
      <Alert variant="warning" isInline title="Revert requested">
        The request was created as {name}, but its progress could not be read:{' '}
        {String((loadError as Error)?.message ?? loadError)}
      </Alert>
    );
  }
  if (phase === 'Succeeded') {
    return (
      <Alert variant="success" isInline title="Reverted">
        {message}
        {steps.length > 0 && (
          <ul>
            {steps.map((step) => (
              <li key={step}>{step}</li>
            ))}
          </ul>
        )}
      </Alert>
    );
  }
  if (phase === 'Failed') {
    return (
      <Alert variant="danger" isInline title="Revert refused">
        {message}
      </Alert>
    );
  }
  if (phase === 'Skipped') {
    return (
      <Alert variant="info" isInline title="Nothing to revert">
        {message}
      </Alert>
    );
  }
  return (
    <Alert variant="info" isInline title="Revert requested">
      {loaded ? 'Waiting for Chronos to apply it…' : 'Loading…'}
    </Alert>
  );
};

export interface OwnerRef {
  kind: string;
  name: string;
}

export const RevertModal: React.FC<{
  isOpen: boolean;
  target: TargetObjectReference;
  namespace?: string;
  version?: Version;
  redacted?: boolean;
  ownerRef?: OwnerRef;
  onClose: () => void;
}> = ({ isOpen, target, namespace, version, redacted, ownerRef, onClose }) => {
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [created, setCreated] = React.useState<string | null>(null);

  const time = version ? new Date(version.time).toLocaleString() : '';

  const submit = async (): Promise<void> => {
    if (!version?.snapshot) {
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const ro = await k8sCreate<RevertOperation>({
        model: RevertOperationModel,
        data: {
          apiVersion: 'chronos.ocp.run/v1alpha1',
          kind: 'RevertOperation',
          metadata: {
            generateName: `revert-${target.name}-`
              .toLowerCase()
              .replace(/[^a-z0-9-]/g, '-'),
            namespace,
          },
          spec: {
            target: {
              apiVersion: target.apiVersion,
              kind: target.kind,
              namespace: target.namespace,
              name: target.name,
            },
            toSnapshot: version.snapshot,
            strategy: 'ServerSideApply',
          },
        },
      });
      setCreated(ro?.metadata?.name ?? '');
    } catch (e) {
      setError(String((e as Error)?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  const handleClose = (): void => {
    setCreated(null);
    setError(null);
    onClose();
  };

  return (
    <Modal isOpen={isOpen} onClose={handleClose} variant="small">
      <ModalHeader title={`Revert ${target.kind}/${target.name}?`} />
      <ModalBody>
        {created !== null ? (
          <RevertOutcome name={created} namespace={namespace} />
        ) : (
          <>
            <p>
              This restores <b>{target.kind}/{target.name}</b> to its state from{' '}
              <b>{time}</b> using server-side apply.
            </p>
            {ownerRef && (
              <Alert
                variant="warning"
                isInline
                title="Managed by a controller"
                className="chronos-drawer-section"
              >
                This object is owned by {ownerRef.kind}/{ownerRef.name} and will
                likely be reconciled back to the operator&apos;s desired state.
              </Alert>
            )}
            {redacted && (
              <Alert
                variant="info"
                isInline
                title="Redacted values won't be restored"
                className="chronos-drawer-section"
              >
                Credential values (Secret data, passwords, tokens, keys) were
                never stored. Those fields are left exactly as they are now;
                everything else is restored. The result lists what was left
                alone.
              </Alert>
            )}
            {error && (
              <Alert
                variant="danger"
                isInline
                title="Revert failed"
                className="chronos-drawer-section"
              >
                {error}
              </Alert>
            )}
          </>
        )}
      </ModalBody>
      <ModalFooter>
        {created !== null ? (
          <Button variant="primary" onClick={handleClose}>
            Close
          </Button>
        ) : (
          <>
            <Button
              variant="danger"
              onClick={() => { void submit(); }}
              isLoading={busy}
              isDisabled={busy || !version?.snapshot}
            >
              Revert
            </Button>
            <Button variant="link" onClick={handleClose}>
              Cancel
            </Button>
          </>
        )}
      </ModalFooter>
    </Modal>
  );
};
