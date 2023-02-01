import glob
import importlib
import ipaddress
import itertools
import json
import logging
import math
import os
import re
import signal
import string
import time
import traceback
import uuid
from types import FrameType
from typing import Generator, Optional, Tuple

import inflection
import magic  # type: ignore
import redis
import validators  # type: ignore
import yara  # type: ignore
from boltons import iterutils  # type: ignore
from tldextract import TLDExtract  # type: ignore


class RequestTimeout(Exception):
    """Raised when request times out."""

    pass


class DistributionTimeout(Exception):
    """Raised when file distribution times out."""

    pass


class ScannerTimeout(Exception):
    """Raised when scanner times out."""

    pass


class ScannerException(Exception):
    def __init__(self, message=""):
        self.message = message
        super().__init__(self.message)


class File(object):
    """Defines a file that will be scanned.

    This object contains metadata that describes files input into the
    system. The object should only contain data is that is not stored
    elsewhere (e.g. file bytes stored in Redis). In future releases this
    object may be removed in favor of a pure-Redis design.

    Attributes:
        data: Byte string of file data for local-only use
        depth: Integer that represents how deep the file was embedded.
        flavors: Dictionary of flavors assigned to the file during distribution.
        name: String that contains the name of the file.
        parent: UUIDv4 of the file that produced this file.
        pointer: String that contains the location of the file bytes in Redis.
        size: Integer of data length
        source: String that describes which scanner the file originated from.
        tree: Dictionary of relationships between File objects
        uid: String that contains a universally unique identifier (UUIDv4) used to uniquely identify the file.
    """

    # FIXME: There doesn't appear to be any reason why pointer and uid should be different
    def __init__(
        self,
        pointer: str = "",
        parent: str = "",
        depth: int = 0,
        name: str = "",
        source: str = "",
        data: Optional[bytes] = None,
    ) -> None:
        """Inits file object."""
        self.data: Optional[bytes] = data
        self.depth: int = depth
        self.flavors: dict = {}
        self.name: str = name
        self.parent: str = parent
        self.pointer: str = pointer
        self.scanners: list[str] = []
        self.size: int = -1
        self.source: str = source
        self.tree: dict = {}
        self.uid = str(uuid.uuid4())

        if not self.pointer:
            self.pointer = self.uid

    def dictionary(self) -> dict:

        return {
            "depth": self.depth,
            "flavors": self.flavors,
            "name": self.name,
            "scanners": self.scanners,
            "size": self.size,
            "source": self.source,
            "tree": self.tree,
        }

    def add_flavors(self, flavors: dict) -> None:
        """Adds flavors to the file.

        In cases where flavors and self.flavors share duplicate keys, flavors
        will overwrite the duplicate value.
        """
        self.flavors = {**self.flavors, **flavors}


def timeout_handler(ex):
    """Signal timeout handler"""

    def fn(signal_number: int, frame: Optional[FrameType]):
        raise ex

    return fn


class Backend(object):
    def __init__(
        self, backend_cfg: dict, coordinator: Optional[redis.StrictRedis] = None
    ) -> None:
        self.scanner_cache: dict = {}
        self.backend_cfg: dict = backend_cfg
        self.coordinator: Optional[redis.StrictRedis] = coordinator
        self.limits: dict = backend_cfg.get("limits", {})
        self.scanners: dict = backend_cfg.get("scanners", {})

        self.compiled_magic = magic.Magic(
            magic_file=backend_cfg.get("tasting", {}).get("mime_db", ""),
            mime=True,
        )

        yara_rules = backend_cfg.get("tasting", {}).get(
            "yara_rules", "/etc/strelka/taste/"
        )
        if os.path.isdir(yara_rules):
            yara_filepaths = {}
            globbed_yara = glob.iglob(
                f"{yara_rules}/**/*.yar*",
                recursive=True,
            )
            for (i, entry) in enumerate(globbed_yara):
                yara_filepaths[f"namespace{i}"] = entry
            self.compiled_yara = yara.compile(filepaths=yara_filepaths)
        else:
            self.compiled_yara = yara.compile(filepath=yara_rules)

    def taste_mime(self, data: bytes) -> list:
        """Tastes file data with libmagic."""
        return [self.compiled_magic.from_buffer(data)]

    def taste_yara(self, data: bytes) -> list:
        """Tastes file data with YARA."""
        encoded_whitespace = string.whitespace.encode()
        stripped_data = data.lstrip(encoded_whitespace)
        yara_matches = self.compiled_yara.match(data=stripped_data)
        return [match.rule for match in yara_matches]

    def match_flavors(self, data: bytes) -> dict:
        return {"mime": self.taste_mime(data), "yara": self.taste_yara(data)}

    def work(self) -> None:
        """Process tasks from Redis coordinator"""

        logging.info("starting up")

        if not self.coordinator:
            logging.error("no coordinator specified")
            return

        count = 0
        work_start = time.time()
        work_expire = work_start + self.limits.get("time_to_live", 900)

        while True:
            if self.limits.get("max_files") != 0:
                if count >= self.limits.get("max_files", 5000):
                    break
            if self.limits.get("time_to_live") != 0:
                if time.time() >= work_expire:
                    break

            # Retrieve request task from Redis coordinator
            if (
                self.backend_cfg.get("coordinator", {"mode": "polling"}).get(
                    "mode", "polling"
                )
                == "polling"
            ):
                task = self.coordinator.zpopmin("tasks", count=1)
                # Get request metadata and Redis context deadline UNIX timestamp
                if not task:
                    time.sleep(
                        self.backend_cfg.get(
                            "coordinator", {"wait-timeout-sec": 0.250}
                        ).get("wait-timeout-sec", 0.250)
                        * 100
                    )
                    continue
                task = task[0]
                (task_item, expire_at) = task
            elif (
                self.backend_cfg.get("coordinator", {"mode": "polling"}).get(
                    "mode", "polling"
                )
                == "blocking"
            ):
                task = self.coordinator.bzpopmin(
                    "tasks",
                    timeout=self.backend_cfg.get(
                        "coordinator", {"wait-timeout-sec": 60}
                    ).get("wait-timeout-sec", 60),
                )
                if not task:
                    continue
                # Get request metadata and Redis context deadline UNIX timestamp
                (_, task_item, expire_at) = task
            else:
                raise Exception("No valid coordinator mode")

            # Parse request JSON
            try:
                task_info = json.loads(task_item)
                root_id = task_info["id"]
                file = File(pointer=root_id, name=task_info["attributes"]["filename"])
            except json.JSONDecodeError:
                logging.error("Failed to parse task_info JSON")
                continue
            except KeyError as ex:
                logging.error(
                    f"No filename attached (error: {ex}) to request: {task_item}"
                )
                continue

            expire_at = math.ceil(expire_at)
            timeout = math.ceil(expire_at - time.time())

            # If the deadline has passed, bail out
            if timeout <= 0:
                continue

            try:
                # Prepare timeout handler
                signal.signal(signal.SIGALRM, timeout_handler(RequestTimeout))
                signal.alarm(timeout)

                # Distribute the file to the scanners
                self.distribute(root_id, file, expire_at)

                # Push completed event back to Redis to complete request
                p = self.coordinator.pipeline(transaction=False)
                p.rpush(f"event:{root_id}", "FIN")
                p.expireat(f"event:{root_id}", expire_at)
                p.execute()

                # Reset timeout handler
                signal.alarm(0)

            except RequestTimeout:
                logging.debug(f"request {root_id} timed out")
            except Exception:
                signal.alarm(0)
                logging.exception("unknown exception (see traceback below)")

            count += 1

        logging.info(
            f"shutdown after scanning {count} file(s) and"
            f" {time.time() - work_start} second(s)"
        )

    def distribute(self, root_id: str, file: File, expire_at: int) -> list[dict]:
        """Distributes a file through scanners.

        Args:
            root_id: Root request/file UUIDv4
            file: File object
            expire_at: Deadline UNIX timestamp
        Returns:
            List of event dictionaries
        """

        try:
            data = b""
            files = []
            events = []

            pipeline = None

            try:
                # Prepare timeout handler
                signal.signal(signal.SIGALRM, timeout_handler(DistributionTimeout))
                signal.alarm(self.limits.get("distribution", 600))

                if file.depth > self.limits.get("max_depth", 15):
                    logging.info(f"request {root_id} exceeded maximum depth")
                    return []

                # Distribute can work local-only (data in File) or through a coordinator
                if file.data:
                    # Pull data for file from File object
                    data = file.data
                elif self.coordinator:
                    # Pull data for file from coordinator
                    while True:
                        pop = self.coordinator.lpop(f"data:{file.pointer}")
                        if pop is None:
                            break
                        data += pop

                    # Initialize Redis pipeline
                    pipeline = self.coordinator.pipeline(transaction=False)
                else:
                    raise Exception("No data or coordinator available")

                # Match data to mime and yara flavors
                file.add_flavors(self.match_flavors(data))

                # Get list of matching scanners
                scanner_list = self.match_scanners(file)

                tree_dict = {
                    "node": file.uid,
                    "parent": file.parent,
                    "root": root_id,
                }

                # Since root_id comes from the request, use that instead of the file's uid
                if file.depth == 0:
                    tree_dict["node"] = root_id
                if file.depth == 1:
                    tree_dict["parent"] = root_id

                # Update the file object
                file.scanners = [s.get("name") for s in scanner_list]
                file.size = len(data)
                file.tree = tree_dict

                scan: dict = {}

                for scanner in scanner_list:
                    try:
                        name = scanner["name"]
                        und_name = inflection.underscore(name)
                        scanner_import = f"strelka.scanners.{und_name}"
                        module = importlib.import_module(scanner_import)

                        if self.backend_cfg.get("caching", {"scanner": True}).get(
                            "scanner", True
                        ):
                            # Cache a copy of each scanner object
                            if und_name not in self.scanner_cache:
                                attr = getattr(module, name)(
                                    self.backend_cfg, self.coordinator
                                )
                                self.scanner_cache[und_name] = attr
                            plugin = self.scanner_cache[und_name]

                            # Clear cached scanner of files
                            plugin.files = []
                            plugin.flags = []
                        else:
                            plugin = getattr(module, name)(
                                self.backend_cfg, self.coordinator
                            )

                        options = scanner.get("options", {})

                        # Run the scanner
                        (scanner_files, scanner_event) = plugin.scan_wrapper(
                            data,
                            file,
                            options,
                            expire_at,
                        )

                        # Collect extracted files
                        files.extend(scanner_files)

                        scan = {
                            **scan,
                            **scanner_event,
                        }

                    except ModuleNotFoundError:
                        logging.exception(
                            f'scanner {scanner.get("name", "__missing__")} not found'
                        )

                event = {
                    **{"file": file.dictionary()},
                    **{"scan": scan},
                }

                # Collect events for local-only
                events.append(event)

                # Send event back to Redis coordinator
                if pipeline:
                    pipeline.rpush(f"event:{root_id}", format_event(event))
                    pipeline.expireat(f"event:{root_id}", expire_at)
                    pipeline.execute()

                signal.alarm(0)

            except DistributionTimeout:
                # FIXME: node id is not always file.uid
                logging.exception(f"node {file.uid} timed out")

            # Re-ingest extracted files
            for scanner_file in files:
                scanner_file.parent = file.uid
                scanner_file.depth = file.depth + 1
                events.extend(self.distribute(root_id, scanner_file, expire_at))

        except RequestTimeout:
            signal.alarm(0)
            raise

        return events

    def match_scanner(
        self,
        scanner: str,
        mappings: list,
        file: File,
        ignore_wildcards: Optional[bool] = False,
    ) -> dict:
        """Matches a scanner to mappings and file data.

        Performs the task of assigning a scanner based on the scan configuration
        mappings and file flavors, filename, and source. Assignment supports
        positive and negative matching: scanners are assigned if any positive
        categories are matched and no negative categories are matched. Flavors are
        literal matches, filename and source matches uses regular expressions.

        Args:
            scanner: Name of the scanner to be assigned.
            mappings: List of dictionaries that contain values used to assign
                the scanner.
            file: File object to use during scanner assignment.
            ignore_wildcards: Filter out wildcard scanner matches
        Returns:
            Dictionary containing the assigned scanner or None.
        """
        for mapping in mappings:
            negatives = mapping.get("negative", {})
            positives = mapping.get("positive", {})
            neg_flavors = negatives.get("flavors", [])
            neg_filename = negatives.get("filename", None)
            neg_source = negatives.get("source", None)
            pos_flavors = positives.get("flavors", [])
            pos_filename = positives.get("filename", None)
            pos_source = positives.get("source", None)
            assigned = {
                "name": scanner,
                "priority": mapping.get("priority", 5),
                "options": mapping.get("options", {}),
            }

            for neg_flavor in neg_flavors:
                if neg_flavor in itertools.chain(*file.flavors.values()):
                    return {}
            if neg_filename is not None:
                if re.search(neg_filename, file.name) is not None:
                    return {}
            if neg_source is not None:
                if re.search(neg_source, file.source) is not None:
                    return {}
            for pos_flavor in pos_flavors:
                if (
                    pos_flavor == "*" and not ignore_wildcards
                ) or pos_flavor in itertools.chain(*file.flavors.values()):
                    return assigned
            if pos_filename is not None:
                if re.search(pos_filename, file.name) is not None:
                    return assigned
            if pos_source is not None:
                if re.search(pos_source, file.source) is not None:
                    return assigned

        return {}

    def match_scanners(
        self, file: File, ignore_wildcards: Optional[bool] = False
    ) -> list:
        """
        Wraps match_scanner

        Args:
            file: File object to use during scanner assignment.
            ignore_wildcards: Filter out wildcard scanner matches.
        Returns:
            List of scanner dictionaries.
        """
        scanner_list = []

        for name in self.scanners:
            mappings = self.scanners.get(name, {})
            scanner = self.match_scanner(name, mappings, file, ignore_wildcards)
            if scanner:
                scanner_list.append(scanner)

        scanner_list.sort(
            key=lambda k: k.get("priority", 5),
            reverse=True,
        )

        return scanner_list


class IocOptions(object):
    """
    Defines an ioc options object that can be used to specify the ioc_type for developers as opposed to using a
    string.
    """

    domain = "domain"
    url = "url"
    md5 = "md5"
    sha1 = "sha1"
    sha256 = "sha256"
    email = "email"
    ip = "ip"


class Scanner(object):
    """Defines a scanner that scans File objects.

    Each scanner inherits this class and overrides methods (init and scan)
    to perform scanning functions.

    Attributes:
        name: String that contains the scanner class name.
            This is referenced in the scanner metadata.
        key: String that contains the scanner's metadata key.
            This is used to identify the scanner metadata in scan results.
        event: Dictionary containing the result of scan
        backend_cfg: Dictionary that contains the parsed backend configuration.
        scanner_timeout: Amount of time (in seconds) that a scanner can spend
            scanning a file. Can be overridden on a per-scanner basis
            (see scan_wrapper).
        coordinator: Redis client connection to the coordinator.
    """

    def __init__(
        self, backend_cfg: dict, coordinator: Optional[redis.StrictRedis] = None
    ) -> None:
        """Inits scanner with scanner name and metadata key."""
        self.name = self.__class__.__name__
        self.key = inflection.underscore(self.name.replace("Scan", ""))
        self.scanner_timeout = backend_cfg.get("limits", {}).get("scanner", 10)
        self.coordinator = coordinator
        self.event: dict = dict()
        self.files: list = []
        self.flags: list = []
        self.iocs: list = []
        self.type = IocOptions
        self.extract = TLDExtract(suffix_list_urls=[])
        self.expire_at: int = 0
        self.init()

    def init(self) -> None:
        """Overrideable init.

        This method can be used to setup one-time variables required
        during scanning."""

    def timeout_handler(self, signal_number: int, frame: Optional[FrameType]) -> None:
        """Signal ScannerTimeout"""
        raise ScannerTimeout

    def scan(self, data, file, options, expire_at) -> None:
        """Overrideable scan method.

        Args:
            data: Data associated with file that will be scanned.
            file: File associated with data that will be scanned (see File()).
            options: Options to be applied during scan.
            expire_at: Expiration date for any files extracted during scan.
        """
        pass

    def scan_wrapper(
        self, data: bytes, file: File, options: dict, expire_at: int
    ) -> Tuple[list[File], dict]:
        """Sets up scan attributes and calls scan method.

        Scanning code is wrapped in try/except for error handling.
        The scanner always returns a list of extracted files (which may be
        empty) and metadata regardless of whether the scanner completed
        successfully or hit an exception.

        Args:
            data: Data associated with file that will be scanned.
            file: File associated with data that will be scanned (see File()).
            options: Options to be applied during scan.
            expire_at: Expiration date for any files extracted during scan.
        Returns:
            List of extracted File objects (may be empty).
            Dictionary of scanner metadata.
        Raises:
            DistributionTimeout: interrupts the scan when distribution times out.
            RequestTimeout: interrupts the scan when request times out.
            Exception: Unknown exception occurred.
        """
        start = time.time()
        self.event = dict()
        self.scanner_timeout = options.get(
            "scanner_timeout", self.scanner_timeout or 10
        )

        try:
            signal.signal(signal.SIGALRM, self.timeout_handler)
            signal.alarm(self.scanner_timeout)
            self.expire_at = expire_at
            self.scan(data, file, options, expire_at)
            signal.alarm(0)
        except ScannerTimeout:
            self.flags.append("timed_out")
        except (DistributionTimeout, RequestTimeout):
            raise
        except ScannerException as e:
            signal.alarm(0)
            self.event.update({"exception": e.message})
        except Exception as e:
            signal.alarm(0)
            logging.exception(
                f"{self.name}: unhandled exception while scanning"
                f' uid {file.uid if file else "_missing_"} (see traceback below)'
            )
            self.flags.append("uncaught_exception")
            self.event.update(
                {"exception": "\n".join(traceback.format_exception(e, limit=-10))}
            )

        self.event = {
            **{"elapsed": round(time.time() - start, 6)},
            **{"flags": self.flags},
            **self.event,
        }
        return (self.files, {self.key: self.event})

    def emit_file(
        self, data: bytes, name: str = "", flavors: Optional[list[str]] = None
    ) -> None:
        """Re-ingest extracted file"""
        extract_file = File(
            name=name,
            source=self.name,
        )
        if flavors:
            extract_file.add_flavors({"external": flavors})

        if self.coordinator:
            for c in chunk_string(data):
                self.upload_to_coordinator(
                    extract_file.pointer,
                    c,
                    self.expire_at,
                )
        else:
            extract_file.data = data
        self.files.append(extract_file)

    def upload_to_coordinator(self, pointer, chunk, expire_at) -> None:
        """Uploads data to coordinator.

        This method is used during scanning to upload data to coordinator,
        where the data is later pulled from during file distribution.

        Args:
            pointer: String that contains the location of the file bytes
                in Redis.
            chunk: String that contains a chunk of data to be added to
                the coordinator.
            expire_at: Expiration date for data stored in pointer.
        """
        if self.coordinator:
            p = self.coordinator.pipeline(transaction=False)
            p.rpush(f"data:{pointer}", chunk)
            p.expireat(f"data:{pointer}", expire_at)
            p.execute()

    def process_ioc(
        self, ioc, ioc_type, scanner_name, description="", malicious=False
    ) -> None:
        if not ioc:
            return
        if ioc_type == "url":
            if validators.ipv4(self.extract(ioc).domain):
                self.process_ioc(
                    self.extract(ioc).domain, "ip", scanner_name, description, malicious
                )
            else:
                self.process_ioc(
                    self.extract(ioc).registered_domain,
                    "domain",
                    scanner_name,
                    description,
                    malicious,
                )
            if not validators.url(ioc):
                logging.warning(f"{ioc} is not a valid url")
                return
        elif ioc_type == "ip":
            try:
                ipaddress.ip_address(ioc)
            except ValueError:
                logging.warning(f"{ioc} is not a valid IP")
                return
        elif ioc_type == "domain":
            if not validators.domain(ioc):
                logging.warning(f"{ioc} is not a valid domain")
                return
        elif ioc_type == "email":
            if not validators.email(ioc):
                logging.warning(f"{ioc} is not a valid email")
                return

        if malicious:
            self.iocs.append(
                {
                    "ioc": ioc,
                    "ioc_type": ioc_type,
                    "scanner": scanner_name,
                    "description": description,
                    "malicious": True,
                }
            )
        else:
            self.iocs.append(
                {
                    "ioc": ioc,
                    "ioc_type": ioc_type,
                    "scanner": scanner_name,
                    "description": description,
                }
            )

    def add_iocs(self, ioc, ioc_type, description="", malicious=False) -> None:
        """Adds ioc to the iocs.
        :param ioc: The IOC or list of IOCs to be added. All iocs must be of the same type. Must be type String or Bytes.
        :param ioc_type: Must be one of md5, sha1, sha256, domain, url, email, ip, either as string or type object (e.g. self.type.domain).
        :param description (Optional): Description of the IOCs.
        :param malicious (Optional): Reasonable determination whether the indicator is or would be used maliciously. Example:
          Malware Command and Control. Should not be used solely for determining maliciousness since testing values may be present.
        """
        try:
            accepted_iocs = ["md5", "sha1", "sha256", "domain", "url", "email", "ip"]
            if ioc_type not in accepted_iocs:
                logging.warning(
                    f"{ioc_type} not in accepted range. Acceptable ioc types are: {accepted_iocs}"
                )
                return
            if isinstance(ioc, list):
                for i in ioc:
                    if isinstance(i, bytes):
                        i = i.decode()
                    if not isinstance(i, str):
                        logging.warning(
                            f"Could not process {i} from {self.name}: Type {type(i)} is not type Bytes or String"
                        )
                        continue
                    self.process_ioc(
                        i,
                        ioc_type,
                        self.name,
                        description=description,
                        malicious=malicious,
                    )
            else:
                if isinstance(ioc, bytes):
                    ioc = ioc.decode()
                if not isinstance(ioc, str):
                    logging.warning(
                        f"Could not process {ioc} from {self.name}: Type {type(ioc)} is not type Bytes or String"
                    )
                    return
                self.process_ioc(
                    ioc,
                    ioc_type,
                    self.name,
                    description=description,
                    malicious=malicious,
                )
        except Exception as e:
            logging.error(f"Failed to add {ioc} from {self.name}: {e}")


def chunk_string(s, chunk=1024 * 16) -> Generator[bytes, None, None]:
    """Takes an input string and turns it into smaller byte pieces.

    This method is required for inserting data into coordinator.

    Yields:
        Chunks of the input string.
    """
    if isinstance(s, bytearray):
        s = bytes(s)

    for c in range(0, len(s), chunk):
        yield s[c : c + chunk]


def format_event(metadata: dict) -> str:
    """Formats file metadata into an event.

    This function must be used on file metadata before the metadata is
    pushed to Redis. The function takes a dictionary containing a
    complete file event and runs the following (sequentially):
        * Replaces all bytes with strings
        * Removes all values that are empty strings, empty lists,
            empty dictionaries, or None
        * Dumps dictionary as JSON

    Args:
        metadata: Dictionary that needs to be formatted into an event.

    Returns:
        JSON-formatted file event.
    """

    def visit(path, key, value):
        if isinstance(value, (bytes, bytearray)):
            value = str(value, encoding="UTF-8", errors="replace")
        return key, value

    remap1 = iterutils.remap(metadata, visit=visit)
    remap2 = iterutils.remap(
        remap1,
        lambda p, k, v: v != "" and v != [] and v != {} and v is not None,
    )
    return json.dumps(remap2)
