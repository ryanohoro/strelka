# -----------------------------------------------------------------------------
# Copyright (c) 2013-2023, PyInstaller Development Team.
#
# Distributed under the terms of the GNU General Public License (version 2
# or later) with exception for distributing the bootloader.
#
# The full license is in the file COPYING.txt, distributed with this software.
#
# SPDX-License-Identifier: (GPL-2.0-or-later WITH Bootloader-exception)
# -----------------------------------------------------------------------------
"""
Python-based CArchive (PKG) reader implementation. Used only in the archive_viewer utility.
"""

import io
import os
import struct

from strelka.auxiliary.pyinstaller.pyimod01_archive import (
    ArchiveReadError,
    ZlibArchiveReader,
)


class NotAnArchiveError(TypeError):
    pass


# Type codes for CArchive TOC entries
PKG_ITEM_BINARY = "b"  # binary
PKG_ITEM_DEPENDENCY = "d"  # runtime option
PKG_ITEM_PYZ = "z"  # zlib (pyz) - frozen Python code
PKG_ITEM_ZIPFILE = "Z"  # zlib (pyz) - frozen Python code
PKG_ITEM_PYPACKAGE = "M"  # Python package (__init__.py)
PKG_ITEM_PYMODULE = "m"  # Python module
PKG_ITEM_PYSOURCE = "s"  # Python script (v3)
PKG_ITEM_DATA = "x"  # data
PKG_ITEM_RUNTIME_OPTION = "o"  # runtime option
PKG_ITEM_SPLASH = "l"  # splash resources


class CArchiveReader:
    """
    Reader for PyInstaller's CArchive (PKG) archive.
    """

    # Cookie - holds some information for the bootloader. C struct format definition. '!' at the beginning means network
    # byte order. C struct looks like:
    #
    # typedef struct _archive_cookie
    # {
    #     char magic[8];
    #     uint32_t pkg_length;
    #     uint32_t toc_offset;
    #     uint32_t toc_length;
    #     uint32_t python_version;
    #     char python_libname[64];
    # } ARCHIVE_COOKIE;
    #
    _COOKIE_MAGIC_PATTERN = b"MEI\014\013\012\013\016"

    _COOKIE_FORMAT = "!8sIIII64s"
    _COOKIE_LENGTH = struct.calcsize(_COOKIE_FORMAT)

    # TOC entry:
    #
    # typedef struct _toc_entry
    # {
    #     uint32_t entry_length;
    #     uint32_t offset;
    #     uint32_t length;
    #     uint32_t uncompressed_length;
    #     unsigned char compression_flag;
    #     char typecode;
    #     char name[1]; /* Variable-length name, padded to multiple of 16 */
    # } TOC_ENTRY;
    #
    _TOC_ENTRY_FORMAT = "!IIIIBc"
    _TOC_ENTRY_LENGTH = struct.calcsize(_TOC_ENTRY_FORMAT)

    def __init__(self, data):
        self._data = data
        self._start_offset = 0
        self._toc_offset = 0
        self._toc_length = 0

        self.toc = {}
        self.options = []

        # Load TOC
        fp = io.BytesIO(data)
        # Find cookie MAGIC pattern
        cookie_start_offset = self._find_magic_pattern(fp, self._COOKIE_MAGIC_PATTERN)
        if cookie_start_offset == -1:
            raise ArchiveReadError("Could not find COOKIE magic pattern!")

        # Read the whole cookie
        fp.seek(cookie_start_offset, os.SEEK_SET)
        cookie_data = fp.read(self._COOKIE_LENGTH)

        magic, archive_length, toc_offset, toc_length, pyvers, pylib_name = (
            struct.unpack(self._COOKIE_FORMAT, cookie_data)
        )

        # Parse the cookie
        self._COOKIE = {
            "magic": magic.decode("utf-8", errors="ignore"),
            "archive_length": archive_length,
            "toc_offset": toc_offset,
            "toc_length": toc_length,
            "pyvers": pyvers,
            "pylib_name": pylib_name.rstrip(b"\0").decode("utf-8", errors="ignore"),
        }

        # Compute start of the archive
        self._start_offset = (
            cookie_start_offset + self._COOKIE_LENGTH
        ) - archive_length

        # Verify that Python shared library name is set
        if not pylib_name:
            raise ArchiveReadError("Python shared library name not set in the archive!")

        # Read whole toc
        fp.seek(self._start_offset + toc_offset)
        toc_data = fp.read(toc_length)

        self.toc, self.options = self._parse_toc(toc_data)

    @staticmethod
    def _find_magic_pattern(fp, magic_pattern):
        # Start at the end of file, and scan back-to-start
        fp.seek(0, os.SEEK_END)
        end_pos = fp.tell()

        # Scan from back
        SEARCH_CHUNK_SIZE = 8192
        magic_offset = -1
        while end_pos >= len(magic_pattern):
            start_pos = max(end_pos - SEARCH_CHUNK_SIZE, 0)
            chunk_size = end_pos - start_pos
            # Is the remaining chunk large enough to hold the pattern?
            if chunk_size < len(magic_pattern):
                break
            # Read and scan the chunk
            fp.seek(start_pos, os.SEEK_SET)
            buf = fp.read(chunk_size)
            pos = buf.rfind(magic_pattern)
            if pos != -1:
                magic_offset = start_pos + pos
                break
            # Adjust search location for next chunk; ensure proper overlap
            end_pos = start_pos + len(magic_pattern) - 1

        return magic_offset

    @classmethod
    def _parse_toc(cls, data):
        options = []
        toc = {}
        cur_pos = 0
        while cur_pos < len(data):
            # Read and parse the fixed-size TOC entry header
            (
                entry_length,
                entry_offset,
                data_length,
                uncompressed_length,
                compression_flag,
                typecode,
            ) = struct.unpack(
                cls._TOC_ENTRY_FORMAT, data[cur_pos : (cur_pos + cls._TOC_ENTRY_LENGTH)]
            )
            cur_pos += cls._TOC_ENTRY_LENGTH
            # Read variable-length name
            name_length = entry_length - cls._TOC_ENTRY_LENGTH
            name, *_ = struct.unpack(
                f"{name_length}s", data[cur_pos : (cur_pos + name_length)]
            )
            cur_pos += name_length
            # Name string may contain up to 15 bytes of padding
            name = name.rstrip(b"\0").decode("utf-8", errors="ignore")

            typecode = typecode.decode("ascii")

            # The TOC should not contain duplicates, except for OPTION entries. Therefore, keep those
            # in a separate list. With options, the rest of the entries do not make sense, anyway.
            if typecode == "o":
                options.append(name)
            else:
                toc[name] = (
                    entry_offset,
                    data_length,
                    uncompressed_length,
                    compression_flag,
                    typecode,
                )

        return toc, options

    def extract(self, name):
        """
        Extract data for the given entry name.
        """

        entry = self.toc.get(name)
        if entry is None:
            raise KeyError(f"No entry named {name} found in the archive!")

        entry_offset, data_length, uncompressed_length, compression_flag, typecode = (
            entry
        )
        with io.BytesIO(self._data) as fd:
            fd.seek(self._start_offset + entry_offset, os.SEEK_SET)
            data = fd.read(data_length)

        if compression_flag:
            import zlib

            data = zlib.decompress(data)

        return data

    def open_embedded_archive(self, name):
        """
        Open new archive reader for the embedded archive.
        """

        entry = self.toc.get(name)
        if entry is None:
            raise KeyError(f"No entry named {name} found in the archive!")

        entry_offset, data_length, uncompressed_length, compression_flag, typecode = (
            entry
        )

        if typecode == PKG_ITEM_PYZ:
            # Open as embedded archive, without extraction.
            return ZlibArchiveReader(self._data, self._start_offset + entry_offset)
        elif typecode == PKG_ITEM_ZIPFILE:
            raise NotAnArchiveError("Zipfile archives not supported yet!")
        else:
            raise NotAnArchiveError(
                f"Entry {name} is not a supported embedded archive!"
            )


def pkg_archive_contents(filename, recursive=True):
    """
    List the contents of the PKG / CArchive. If `recursive` flag is set (the default), the contents of the embedded PYZ
    archive is included as well.

    Used by the tests.
    """

    contents = []

    pkg_archive = CArchiveReader(filename)
    for name, toc_entry in pkg_archive.toc.items():
        *_, typecode = toc_entry
        contents.append(name)
        if typecode == PKG_ITEM_PYZ and recursive:
            pyz_archive = pkg_archive.open_embedded_archive(name)
            for name in pyz_archive.toc.keys():
                contents.append(name)

    return contents
