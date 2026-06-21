/**
 * FileTypeIcon (molecule) — design.md §3.2. Maps a filename/extension (or a
 * folder) to a lucide icon: .html→FileCode, .css→Palette, .js→FileJson2,
 * image→Image, folder→Folder/FolderOpen, else→File.
 */

import {
  File,
  FileCode,
  FileJson2,
  FileText,
  Folder,
  FolderOpen,
  Image as ImageIcon,
  Palette,
  type LucideIcon,
} from "lucide-react";
import { cn } from "@/lib/utils";

const EXT_ICON: Record<string, LucideIcon> = {
  html: FileCode,
  htm: FileCode,
  xml: FileCode,
  svg: ImageIcon,
  css: Palette,
  js: FileJson2,
  mjs: FileJson2,
  json: FileJson2,
  map: FileJson2,
  png: ImageIcon,
  jpg: ImageIcon,
  jpeg: ImageIcon,
  gif: ImageIcon,
  webp: ImageIcon,
  avif: ImageIcon,
  ico: ImageIcon,
  txt: FileText,
  md: FileText,
  csv: FileText,
};

export interface FileTypeIconProps {
  name: string;
  isDir?: boolean;
  /** For directories: show the open-folder variant when expanded. */
  open?: boolean;
  className?: string;
}

export function FileTypeIcon({
  name,
  isDir,
  open,
  className,
}: FileTypeIconProps) {
  if (isDir) {
    const Icon = open ? FolderOpen : Folder;
    return (
      <Icon
        className={cn("size-4 text-muted-foreground", className)}
        aria-hidden="true"
      />
    );
  }
  // Pick by lowercased extension; default to a generic File.
  const ext = name.split(".").pop()?.toLowerCase() ?? "";
  const Icon = EXT_ICON[ext] ?? File;
  return (
    <Icon
      className={cn("size-4 text-muted-foreground", className)}
      aria-hidden="true"
    />
  );
}
