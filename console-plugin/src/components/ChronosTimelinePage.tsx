import * as React from 'react';
import {
  Bullseye,
  Drawer,
  DrawerContent,
  DrawerContentBody,
  EmptyState,
  EmptyStateBody,
  PageSection,
  SearchInput,
  Spinner,
  Title,
  ToggleGroup,
  ToggleGroupItem,
  Toolbar,
  ToolbarContent,
  ToolbarGroup,
  ToolbarItem,
} from '@patternfly/react-core';
import {
  NamespaceBar,
  useActiveNamespace,
} from '@openshift-console/dynamic-plugin-sdk';
import type { ChangeEvent, ViewMode } from '../types';
import { useChangeEvents } from '../useChangeEvents';
import { TimelineView } from './TimelineView';
import { ListView } from './ListView';
import { CalendarView } from './CalendarView';
import { ChangeDetailDrawer } from './ChangeDetailDrawer';
import { DiffPanel } from './DiffPanel';
import '../chronos.css';

const matchesQuery = (e: ChangeEvent, query: string): boolean => {
  if (!query) {
    return true;
  }
  const q = query.toLowerCase();
  const sp = e.spec;
  return [
    sp.target.kind,
    sp.target.name,
    sp.target.namespace,
    sp.actor.username,
    sp.summary,
    sp.verb,
  ]
    .filter((v): v is string => Boolean(v))
    .some((v) => v.toLowerCase().includes(q));
};

const ChronosTimelinePage: React.FC = () => {
  const { events, loaded, error } = useChangeEvents();
  const [view, setView] = React.useState<ViewMode>('timeline');
  const [selected, setSelected] = React.useState<ChangeEvent | null>(null);
  const [diffOpen, setDiffOpen] = React.useState(false);
  const [query, setQuery] = React.useState('');

  const handleSelect = React.useCallback((e: ChangeEvent) => {
    setSelected(e);
    setDiffOpen(true);
  }, []);
  const closeAll = React.useCallback(() => {
    setSelected(null);
    setDiffOpen(false);
  }, []);

  const filtered = React.useMemo(
    () => events.filter((e) => matchesQuery(e, query)),
    [events, query],
  );

  // Honor the console's global Project selector (NamespaceBar): when a specific
  // project is selected, scope the timeline to changes targeting that namespace
  // — the simplest way to cut cross-operator noise. "All Projects" shows
  // everything, including cluster-scoped changes.
  const [activeNamespace] = useActiveNamespace();
  const allNamespaces = !activeNamespace || activeNamespace === '#ALL_NS#';
  const visible = React.useMemo(
    () =>
      allNamespaces
        ? filtered
        : filtered.filter((e) => e.spec.target.namespace === activeNamespace),
    [filtered, allNamespaces, activeNamespace],
  );
  const selectedUid = selected?.metadata?.uid;

  const renderBody = (): React.ReactNode => {
    if (error) {
      return (
        <EmptyState titleText="Couldn't load changes" headingLevel="h4">
          <EmptyStateBody>
            {String((error as Error)?.message ?? error)}
          </EmptyStateBody>
        </EmptyState>
      );
    }
    if (!loaded) {
      return (
        <Bullseye>
          <Spinner />
        </Bullseye>
      );
    }
    if (visible.length === 0) {
      return (
        <EmptyState titleText="No changes recorded" headingLevel="h4">
          <EmptyStateBody>
            Chronos hasn&apos;t recorded any changes yet, or none match your
            search.
          </EmptyStateBody>
        </EmptyState>
      );
    }
    if (view === 'timeline') {
      return (
        <TimelineView
          events={visible}
          selectedUid={selectedUid}
          onSelect={handleSelect}
        />
      );
    }
    if (view === 'calendar') {
      return <CalendarView events={visible} onSelect={handleSelect} />;
    }
    return (
      <ListView
        events={visible}
        selectedUid={selectedUid}
        onSelect={handleSelect}
      />
    );
  };

  const summaryPanel = selected ? (
    <ChangeDetailDrawer event={selected} onClose={closeAll} />
  ) : null;
  const diffPanel = selected ? (
    <DiffPanel
      event={selected}
      events={events}
      onClose={() => { setDiffOpen(false); }}
    />
  ) : null;

  return (
    <>
      <NamespaceBar />
      <PageSection>
        <Title headingLevel="h1">Chronos — Timeline</Title>
      </PageSection>
      <PageSection className="chronos-timeline-page">
        <Toolbar>
          <ToolbarContent>
            <ToolbarItem>
              <SearchInput
                aria-label="Search changes"
                placeholder="Search kind, name, actor…"
                value={query}
                onChange={(_e, v) => { setQuery(v); }}
                onClear={() => { setQuery(''); }}
              />
            </ToolbarItem>
            <ToolbarGroup align={{ default: 'alignEnd' }}>
              <ToolbarItem>
                <ToggleGroup aria-label="View mode">
                  <ToggleGroupItem
                    text="Timeline"
                    buttonId="timeline"
                    isSelected={view === 'timeline'}
                    onChange={() => { setView('timeline'); }}
                  />
                  <ToggleGroupItem
                    text="Calendar"
                    buttonId="calendar"
                    isSelected={view === 'calendar'}
                    onChange={() => { setView('calendar'); }}
                  />
                  <ToggleGroupItem
                    text="List"
                    buttonId="list"
                    isSelected={view === 'list'}
                    onChange={() => { setView('list'); }}
                  />
                </ToggleGroup>
              </ToolbarItem>
            </ToolbarGroup>
          </ToolbarContent>
        </Toolbar>
        <Drawer isExpanded={!!selected && diffOpen} position="bottom" isInline>
          <DrawerContent panelContent={diffPanel}>
            <DrawerContentBody>
              <Drawer isExpanded={!!selected} isInline>
                <DrawerContent panelContent={summaryPanel}>
                  <DrawerContentBody>{renderBody()}</DrawerContentBody>
                </DrawerContent>
              </Drawer>
            </DrawerContentBody>
          </DrawerContent>
        </Drawer>
      </PageSection>
    </>
  );
};

export default ChronosTimelinePage;
