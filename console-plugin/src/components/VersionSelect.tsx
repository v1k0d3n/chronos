import * as React from 'react';
import {
  MenuToggle,
  Select,
  SelectList,
  SelectOption,
} from '@patternfly/react-core';
import type { Version } from '../objectHistory';

const timeLabel = (iso: string): string =>
  new Date(iso).toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });

export const VersionSelect: React.FC<{
  label: string;
  versions: Version[];
  selectedKey?: string;
  onChange: (key: string) => void;
}> = ({ label, versions, selectedKey, onChange }) => {
  const [open, setOpen] = React.useState(false);
  const selected = versions.find((v) => v.key === selectedKey);
  const toggleText = selected ? timeLabel(selected.time) : 'Select version';

  return (
    <Select
      isOpen={open}
      onOpenChange={setOpen}
      selected={selectedKey}
      onSelect={(_e, value) => {
        onChange(String(value));
        setOpen(false);
      }}
      toggle={(ref) => (
        <MenuToggle
          ref={ref}
          isExpanded={open}
          onClick={() => { setOpen(!open); }}
          className="chronos-version-toggle"
        >
          <span className="chronos-version-label">{label}</span>
          <span className="chronos-version-value">{toggleText}</span>
        </MenuToggle>
      )}
    >
      <SelectList>
        {versions.map((v) => (
          <SelectOption key={v.key} value={v.key}>
            {timeLabel(v.time)}
            {v.deleted ? ' · deleted' : ''}
          </SelectOption>
        ))}
      </SelectList>
    </Select>
  );
};
