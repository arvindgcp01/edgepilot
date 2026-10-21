/**
 * The design system, in one import.
 *
 * Everything a view renders comes from here. A page that reaches past this
 * module for a one-off control is a page that will look slightly different from
 * every other, and those differences compound into a product that feels
 * assembled rather than designed.
 */
export { Icon, type IconName } from "./icons";
export { Button, IconButton, type ButtonProps, type ButtonVariant } from "./button";
export { Badge, StatusBadge, Code, Kbd, type BadgeTone } from "./badge";
export { Input, Textarea, Select, SearchInput, Field, Checkbox, Switch, Segmented } from "./field";
export { Card, PageHeader, StatStrip, Section, Facts, Avatar, Divider } from "./layout";
export { Tabs } from "./tabs";
export {
  Table, THead, TBody, Tr, Th, Td, SortableTh, useSorted, useSelection, SelectionCheckbox,
  BulkBar, usePagination, Pagination, type SortState, type SortDir,
} from "./table";
export { Modal, ConfirmDialog, Drawer, Menu, Popover, Tooltip, type MenuItem } from "./overlay";
export {
  ToastProvider, useToast, Skeleton, TableSkeleton, EmptyState, ErrorNote, Empty, Spinner, Callout,
} from "./feedback";
