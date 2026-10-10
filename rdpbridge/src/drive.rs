//! Lecteur partagé « Gna » (redirection de lecteur RDP, canal RDPDR).
//!
//! La machine distante voit un lecteur réseau (« Gna sur GNA » sous Windows,
//! `~/thinclient_drives/Gna` avec xrdp) qui correspond au dossier de transfert de la
//! session sur le serveur Gná. Les fichiers envoyés depuis le navigateur y
//! apparaissent ; ceux que l'utilisateur y dépose côté distant deviennent
//! téléchargeables depuis Gná.
//!
//! Sécurité : la machine distante n'est pas digne de confiance. Tous les chemins
//! qu'elle envoie sont résolus composant par composant à l'intérieur du dossier
//! (« .. », « . », flux NTFS « : » et liens symboliques refusés), et la place
//! occupée est plafonnée.

use std::collections::{HashMap, VecDeque};
use std::fs::{self, File, OpenOptions};
use std::io::{Read, Seek, SeekFrom, Write};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};

use ironrdp::core::impl_as_any;
use ironrdp::pdu::{encode_err, PduResult};
use ironrdp::rdpdr::pdu::efs::*;
use ironrdp::rdpdr::pdu::esc::{ScardCall, ScardIoCtlCode};
use ironrdp::rdpdr::pdu::RdpdrPdu;
use ironrdp::rdpdr::RdpdrBackend;
use ironrdp::svc::SvcMessage;

const MAX_DEPTH: usize = 32;
const MAX_READ: u32 = 4 * 1024 * 1024;
const ALLOC_UNIT: u64 = 4096;

#[derive(Debug)]
struct Handle {
    path: PathBuf,
    file: Option<File>,
    listing: Option<VecDeque<String>>,
    delete_on_close: bool,
}

#[derive(Debug)]
pub struct Drive {
    base: PathBuf,
    max_bytes: u64,
    next_id: u32,
    handles: HashMap<u32, Handle>,
}

impl Drive {
    pub fn new(base: PathBuf, max_bytes: u64) -> Self {
        Drive { base, max_bytes, next_id: 1, handles: HashMap::new() }
    }

    /// Chemin RDP (« \dossier\fichier.txt ») → chemin dans le dossier de transfert.
    fn resolve(&self, p: &str) -> Option<PathBuf> {
        let mut out = self.base.clone();
        let mut depth = 0;
        for c in p.split(['\\', '/']) {
            if c.is_empty() {
                continue;
            }
            if c == "." || c == ".." || c.contains(['\0', ':']) || c.len() > 255 {
                return None;
            }
            depth += 1;
            if depth > MAX_DEPTH {
                return None;
            }
            out.push(c);
            // Aucun lien symbolique sur le chemin (ni dossier intermédiaire, ni fichier).
            if let Ok(m) = fs::symlink_metadata(&out) {
                if m.file_type().is_symlink() {
                    return None;
                }
            }
        }
        Some(out)
    }

    fn used(&self) -> u64 {
        fn walk(p: &Path) -> u64 {
            let Ok(rd) = fs::read_dir(p) else { return 0 };
            rd.flatten()
                .map(|e| match e.metadata() {
                    Ok(m) if m.is_dir() => walk(&e.path()),
                    Ok(m) if m.is_file() => m.len(),
                    _ => 0,
                })
                .sum()
        }
        walk(&self.base)
    }

    fn room_for(&self, growth: u64) -> bool {
        growth == 0 || self.used().saturating_add(growth) <= self.max_bytes
    }
}

impl_as_any!(Drive);

fn io(req: DeviceIoRequest, status: NtStatus) -> DeviceIoResponse {
    DeviceIoResponse::new(req, status)
}

fn msg(pdu: RdpdrPdu) -> PduResult<Vec<SvcMessage>> {
    Ok(vec![SvcMessage::from(pdu)])
}

fn filetime(secs: i64) -> i64 {
    secs.saturating_mul(10_000_000).saturating_add(116_444_736_000_000_000)
}

fn attributes(meta: &fs::Metadata, name: &str) -> FileAttributes {
    let mut a = if meta.is_dir() { FileAttributes::FILE_ATTRIBUTE_DIRECTORY } else { FileAttributes::FILE_ATTRIBUTE_ARCHIVE };
    if name.starts_with('.') && name.len() > 1 {
        a |= FileAttributes::FILE_ATTRIBUTE_HIDDEN;
    }
    a
}

fn name_of(p: &Path) -> String {
    p.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default()
}

/// Filtre de recherche Windows (« * » et « ? », sans tenir compte de la casse).
fn matches(pattern: &str, name: &str) -> bool {
    fn rec(p: &[char], n: &[char]) -> bool {
        match (p.first(), n.first()) {
            (None, None) => true,
            (Some('*'), _) => rec(&p[1..], n) || (!n.is_empty() && rec(p, &n[1..])),
            (Some('?'), Some(_)) => rec(&p[1..], &n[1..]),
            (Some(a), Some(b)) => a.to_lowercase().eq(b.to_lowercase()) && rec(&p[1..], &n[1..]),
            _ => false,
        }
    }
    let p: Vec<char> = pattern.chars().collect();
    let n: Vec<char> = name.chars().collect();
    pattern == "*.*" || rec(&p, &n)
}

impl RdpdrBackend for Drive {
    fn handle_server_device_announce_response(&mut self, _: ServerDeviceAnnounceResponse) -> PduResult<()> {
        Ok(())
    }

    fn handle_scard_call(&mut self, _: DeviceControlRequest<ScardIoCtlCode>, _: ScardCall) -> PduResult<()> {
        Ok(())
    }

    fn handle_drive_io_request(&mut self, req: ServerDriveIoRequest) -> PduResult<Vec<SvcMessage>> {
        match req {
            ServerDriveIoRequest::ServerCreateDriveRequest(r) => self.create(r),
            ServerDriveIoRequest::DeviceReadRequest(r) => self.read(r),
            ServerDriveIoRequest::DeviceWriteRequest(r) => self.write(r),
            ServerDriveIoRequest::DeviceCloseRequest(r) => self.close(r),
            ServerDriveIoRequest::ServerDriveQueryInformationRequest(r) => self.query_info(r),
            ServerDriveIoRequest::ServerDriveQueryVolumeInformationRequest(r) => self.query_volume(r),
            ServerDriveIoRequest::ServerDriveQueryDirectoryRequest(r) => self.query_dir(r),
            ServerDriveIoRequest::ServerDriveSetInformationRequest(r) => self.set_info(r),
            ServerDriveIoRequest::DeviceControlRequest(r) => msg(RdpdrPdu::DeviceControlResponse(DeviceControlResponse {
                device_io_reply: io(r.header, NtStatus::SUCCESS),
                output_buffer: None,
            })),
            // Pas de réponse : la demande reste en attente (comme un dossier sans changement).
            ServerDriveIoRequest::ServerDriveNotifyChangeDirectoryRequest(_) => Ok(Vec::new()),
            ServerDriveIoRequest::ServerDriveLockControlRequest(_) => Ok(Vec::new()),
        }
    }
}

impl Drive {
    fn create(&mut self, r: DeviceCreateRequest) -> PduResult<Vec<SvcMessage>> {
        let fail = |r: DeviceCreateRequest, status| {
            msg(RdpdrPdu::DeviceCreateResponse(DeviceCreateResponse {
                device_io_reply: io(r.device_io_request, status),
                file_id: 0,
                information: Information::empty(),
            }))
        };
        let Some(path) = self.resolve(&r.path) else { return fail(r, NtStatus::ACCESS_DENIED) };
        let want_dir = r.create_options.contains(CreateOptions::FILE_DIRECTORY_FILE);
        let want_file = r.create_options.contains(CreateOptions::FILE_NON_DIRECTORY_FILE);
        let disp = r.create_disposition;
        let meta = fs::symlink_metadata(&path).ok();
        let exists = meta.is_some();
        let is_dir = meta.as_ref().is_some_and(|m| m.is_dir());

        if exists && disp == CreateDisposition::FILE_CREATE {
            return fail(r, NtStatus::OBJECT_NAME_COLLISION);
        }
        if !exists && (disp == CreateDisposition::FILE_OPEN || disp == CreateDisposition::FILE_OVERWRITE) {
            return fail(r, NtStatus::NO_SUCH_FILE);
        }
        if exists && is_dir && want_file {
            return fail(r, NtStatus::ACCESS_DENIED);
        }
        if exists && !is_dir && want_dir {
            return fail(r, NtStatus::NOT_A_DIRECTORY);
        }

        let (file, information) = if is_dir || (!exists && want_dir) {
            if !exists && fs::create_dir(&path).is_err() {
                return fail(r, NtStatus::ACCESS_DENIED);
            }
            (None, if exists { Information::FILE_OPENED } else { Information::FILE_SUPERSEDED })
        } else {
            let truncate = exists
                && (disp == CreateDisposition::FILE_SUPERSEDE
                    || disp == CreateDisposition::FILE_OVERWRITE
                    || disp == CreateDisposition::FILE_OVERWRITE_IF);
            let mut o = OpenOptions::new();
            o.read(true).write(true).custom_flags(libc::O_NOFOLLOW);
            if !exists {
                o.create_new(true);
            }
            if truncate {
                o.truncate(true);
            }
            match o.open(&path) {
                Ok(f) => (
                    Some(f),
                    if truncate {
                        Information::FILE_OVERWRITTEN
                    } else if exists {
                        Information::FILE_OPENED
                    } else {
                        Information::FILE_SUPERSEDED
                    },
                ),
                Err(_) => return fail(r, NtStatus::ACCESS_DENIED),
            }
        };

        let id = self.next_id;
        self.next_id = self.next_id.wrapping_add(1).max(1);
        self.handles.insert(
            id,
            Handle {
                path,
                file,
                listing: None,
                delete_on_close: r.create_options.contains(CreateOptions::FILE_DELETE_ON_CLOSE),
            },
        );
        msg(RdpdrPdu::DeviceCreateResponse(DeviceCreateResponse {
            device_io_reply: io(r.device_io_request, NtStatus::SUCCESS),
            file_id: id,
            information,
        }))
    }

    fn read(&mut self, r: DeviceReadRequest) -> PduResult<Vec<SvcMessage>> {
        let len = r.length.min(MAX_READ) as usize;
        let result = self.handles.get_mut(&r.device_io_request.file_id).and_then(|h| h.file.as_mut()).map(|f| {
            f.seek(SeekFrom::Start(r.offset))?;
            let mut buf = vec![0; len];
            let mut n = 0;
            while n < len {
                match f.read(&mut buf[n..])? {
                    0 => break,
                    k => n += k,
                }
            }
            buf.truncate(n);
            Ok::<_, std::io::Error>(buf)
        });
        let (status, data) = match result {
            Some(Ok(d)) => (NtStatus::SUCCESS, d),
            Some(Err(_)) => (NtStatus::UNSUCCESSFUL, Vec::new()),
            None => (NtStatus::NO_SUCH_FILE, Vec::new()),
        };
        msg(RdpdrPdu::DeviceReadResponse(DeviceReadResponse { device_io_reply: io(r.device_io_request, status), read_data: data }))
    }

    fn write(&mut self, r: DeviceWriteRequest) -> PduResult<Vec<SvcMessage>> {
        let id = r.device_io_request.file_id;
        let current = self.handles.get(&id).and_then(|h| h.file.as_ref()).and_then(|f| f.metadata().ok()).map(|m| m.len());
        let status = match current {
            None => NtStatus::NO_SUCH_FILE,
            Some(cur) => {
                let end = r.offset.saturating_add(r.write_data.len() as u64);
                if !self.room_for(end.saturating_sub(cur)) {
                    NtStatus::UNSUCCESSFUL // espace de transfert plein
                } else {
                    let f = self.handles.get_mut(&id).and_then(|h| h.file.as_mut()).unwrap();
                    match f.seek(SeekFrom::Start(r.offset)).and_then(|_| f.write_all(&r.write_data)) {
                        Ok(()) => NtStatus::SUCCESS,
                        Err(_) => NtStatus::UNSUCCESSFUL,
                    }
                }
            }
        };
        let length = if status == NtStatus::SUCCESS { r.write_data.len() as u32 } else { 0 };
        msg(RdpdrPdu::DeviceWriteResponse(DeviceWriteResponse { device_io_reply: io(r.device_io_request, status), length }))
    }

    fn close(&mut self, r: DeviceCloseRequest) -> PduResult<Vec<SvcMessage>> {
        if let Some(h) = self.handles.remove(&r.device_io_request.file_id) {
            if h.delete_on_close && h.path != self.base {
                drop(h.file);
                let _ = if h.path.is_dir() { fs::remove_dir(&h.path) } else { fs::remove_file(&h.path) };
            }
        }
        msg(RdpdrPdu::DeviceCloseResponse(DeviceCloseResponse { device_io_response: io(r.device_io_request, NtStatus::SUCCESS) }))
    }

    fn query_info(&mut self, r: ServerDriveQueryInformationRequest) -> PduResult<Vec<SvcMessage>> {
        let reply = |status, buffer| {
            msg(RdpdrPdu::ClientDriveQueryInformationResponse(ClientDriveQueryInformationResponse {
                device_io_response: io(r.device_io_request.clone(), status),
                buffer,
            }))
        };
        let Some(h) = self.handles.get(&r.device_io_request.file_id) else { return reply(NtStatus::NO_SUCH_FILE, None) };
        let Ok(meta) = fs::symlink_metadata(&h.path) else { return reply(NtStatus::NO_SUCH_FILE, None) };
        let attrs = attributes(&meta, &name_of(&h.path));
        let size = meta.len() as i64;
        let lvl = r.file_info_class_lvl;
        let info = if lvl == FileInformationClassLevel::FILE_BASIC_INFORMATION {
            FileInformationClass::Basic(FileBasicInformation {
                creation_time: filetime(meta.ctime()),
                last_access_time: filetime(meta.atime()),
                last_write_time: filetime(meta.mtime()),
                change_time: filetime(meta.ctime()),
                file_attributes: attrs,
            })
        } else if lvl == FileInformationClassLevel::FILE_STANDARD_INFORMATION {
            FileInformationClass::Standard(FileStandardInformation {
                allocation_size: size,
                end_of_file: size,
                number_of_links: 1,
                delete_pending: if h.delete_on_close { Boolean::True } else { Boolean::False },
                directory: if meta.is_dir() { Boolean::True } else { Boolean::False },
            })
        } else if lvl == FileInformationClassLevel::FILE_ATTRIBUTE_TAG_INFORMATION {
            FileInformationClass::AttributeTag(FileAttributeTagInformation { file_attributes: attrs, reparse_tag: 0 })
        } else {
            return reply(NtStatus::NOT_SUPPORTED, None);
        };
        reply(NtStatus::SUCCESS, Some(info))
    }

    fn query_volume(&mut self, r: ServerDriveQueryVolumeInformationRequest) -> PduResult<Vec<SvcMessage>> {
        let total = self.max_bytes / ALLOC_UNIT;
        let free = self.max_bytes.saturating_sub(self.used()) / ALLOC_UNIT;
        let lvl = r.fs_info_class_lvl;
        let info = if lvl == FileSystemInformationClassLevel::FILE_FS_FULL_SIZE_INFORMATION {
            Some(FileSystemInformationClass::FileFsFullSizeInformation(FileFsFullSizeInformation {
                total_alloc_units: total as i64,
                caller_available_alloc_units: free as i64,
                actual_available_alloc_units: free as i64,
                sectors_per_alloc_unit: 8,
                bytes_per_sector: 512,
            }))
        } else if lvl == FileSystemInformationClassLevel::FILE_FS_SIZE_INFORMATION {
            Some(FileSystemInformationClass::FileFsSizeInformation(FileFsSizeInformation {
                total_alloc_units: total as i64,
                available_alloc_units: free as i64,
                sectors_per_alloc_unit: 8,
                bytes_per_sector: 512,
            }))
        } else if lvl == FileSystemInformationClassLevel::FILE_FS_ATTRIBUTE_INFORMATION {
            Some(FileSystemInformationClass::FileFsAttributeInformation(FileFsAttributeInformation {
                file_system_attributes: FileSystemAttributes::FILE_CASE_SENSITIVE_SEARCH
                    | FileSystemAttributes::FILE_CASE_PRESERVED_NAMES
                    | FileSystemAttributes::FILE_UNICODE_ON_DISK,
                max_component_name_len: 255,
                file_system_name: "FAT32".to_owned(),
            }))
        } else if lvl == FileSystemInformationClassLevel::FILE_FS_VOLUME_INFORMATION {
            Some(FileSystemInformationClass::FileFsVolumeInformation(FileFsVolumeInformation {
                volume_creation_time: filetime(0),
                volume_serial_number: 0x0647_4E41,
                supports_objects: Boolean::False,
                volume_label: "GNA".to_owned(),
            }))
        } else {
            None
        };
        let status = if info.is_some() { NtStatus::SUCCESS } else { NtStatus::NOT_SUPPORTED };
        msg(RdpdrPdu::ClientDriveQueryVolumeInformationResponse(ClientDriveQueryVolumeInformationResponse {
            device_io_reply: io(r.device_io_request, status),
            buffer: info,
        }))
    }

    fn query_dir(&mut self, r: ServerDriveQueryDirectoryRequest) -> PduResult<Vec<SvcMessage>> {
        let id = r.device_io_request.file_id;
        let initial = r.initial_query != 0;
        let none = |r: ServerDriveQueryDirectoryRequest, initial: bool| {
            msg(RdpdrPdu::ClientDriveQueryDirectoryResponse(ClientDriveQueryDirectoryResponse {
                device_io_reply: io(r.device_io_request, if initial { NtStatus::NO_SUCH_FILE } else { NtStatus::NO_MORE_FILES }),
                buffer: None,
            }))
        };
        if !self.handles.contains_key(&id) {
            return none(r, initial);
        }
        if initial {
            // « \dossier\motif » : motif éventuellement avec jokers.
            let (dir, pattern) = match r.path.rsplit_once('\\') {
                Some((d, p)) => (d.to_string(), p.to_string()),
                None => (String::new(), r.path.clone()),
            };
            let Some(dir_path) = self.resolve(&dir) else { return none(r, initial) };
            let mut names: Vec<String> = fs::read_dir(&dir_path)
                .map(|rd| {
                    rd.flatten()
                        .filter(|e| e.file_type().map(|t| !t.is_symlink()).unwrap_or(false))
                        .map(|e| e.file_name().to_string_lossy().into_owned())
                        .filter(|n| matches(&pattern, n))
                        .collect()
                })
                .unwrap_or_default();
            names.sort();
            let h = self.handles.get_mut(&id).unwrap();
            h.listing = Some(names.into_iter().map(|n| dir_path.join(n).to_string_lossy().into_owned()).collect());
        }
        let next = self.handles.get_mut(&id).and_then(|h| h.listing.as_mut()).and_then(VecDeque::pop_front);
        let Some(full) = next else { return none(r, initial) };
        let full = PathBuf::from(full);
        let Ok(meta) = fs::symlink_metadata(&full) else { return none(r, initial) };
        let name = name_of(&full);
        let attrs = attributes(&meta, &name);
        let (c, a, m, size) = (filetime(meta.ctime()), filetime(meta.atime()), filetime(meta.mtime()), meta.len() as i64);
        let lvl = r.file_info_class_lvl;
        let info = if lvl == FileInformationClassLevel::FILE_BOTH_DIRECTORY_INFORMATION {
            FileInformationClass::BothDirectory(FileBothDirectoryInformation::new(c, a, m, c, size, attrs, name))
        } else if lvl == FileInformationClassLevel::FILE_FULL_DIRECTORY_INFORMATION {
            FileInformationClass::FullDirectory(FileFullDirectoryInformation::new(c, a, m, c, size, attrs, name))
        } else if lvl == FileInformationClassLevel::FILE_DIRECTORY_INFORMATION {
            FileInformationClass::Directory(FileDirectoryInformation::new(c, a, m, c, size, attrs, name))
        } else if lvl == FileInformationClassLevel::FILE_NAMES_INFORMATION {
            FileInformationClass::Names(FileNamesInformation::new(name))
        } else {
            return msg(RdpdrPdu::ClientDriveQueryDirectoryResponse(ClientDriveQueryDirectoryResponse {
                device_io_reply: io(r.device_io_request, NtStatus::NOT_SUPPORTED),
                buffer: None,
            }));
        };
        msg(RdpdrPdu::ClientDriveQueryDirectoryResponse(ClientDriveQueryDirectoryResponse {
            device_io_reply: io(r.device_io_request, NtStatus::SUCCESS),
            buffer: Some(info),
        }))
    }

    fn set_info(&mut self, r: ServerDriveSetInformationRequest) -> PduResult<Vec<SvcMessage>> {
        let id = r.device_io_request.file_id;
        let status = match self.handles.get(&id).map(|h| h.path.clone()) {
            None => NtStatus::NO_SUCH_FILE,
            Some(path) => match &r.set_buffer {
                FileInformationClass::Rename(info) => match self.resolve(&info.file_name) {
                    None => NtStatus::ACCESS_DENIED,
                    Some(_) if path == self.base => NtStatus::ACCESS_DENIED,
                    Some(to) if to.exists() && info.replace_if_exists != Boolean::True => NtStatus::OBJECT_NAME_COLLISION,
                    Some(to) => match fs::rename(&path, &to) {
                        Ok(()) => {
                            self.handles.get_mut(&id).unwrap().path = to;
                            NtStatus::SUCCESS
                        }
                        Err(_) => NtStatus::ACCESS_DENIED,
                    },
                },
                FileInformationClass::Disposition(info) => {
                    if path == self.base {
                        NtStatus::ACCESS_DENIED
                    } else if info.delete_pending != 0 && path.is_dir() && fs::read_dir(&path).map(|mut d| d.next().is_some()).unwrap_or(false) {
                        NtStatus::DIRECTORY_NOT_EMPTY
                    } else {
                        self.handles.get_mut(&id).unwrap().delete_on_close = info.delete_pending != 0;
                        NtStatus::SUCCESS
                    }
                }
                FileInformationClass::EndOfFile(info) => {
                    let new_len = info.end_of_file.max(0) as u64;
                    let cur = fs::metadata(&path).map(|m| m.len()).unwrap_or(0);
                    if !self.room_for(new_len.saturating_sub(cur)) {
                        NtStatus::UNSUCCESSFUL
                    } else {
                        match self.handles.get(&id).and_then(|h| h.file.as_ref()).map(|f| f.set_len(new_len)) {
                            Some(Ok(())) => NtStatus::SUCCESS,
                            _ => NtStatus::UNSUCCESSFUL,
                        }
                    }
                }
                // Dates, taille réservée : sans effet ici.
                _ => NtStatus::SUCCESS,
            },
        };
        msg(RdpdrPdu::ClientDriveSetInformationResponse(
            ClientDriveSetInformationResponse::new(&r, status).map_err(|e| encode_err!(e))?,
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn chemins_confines() {
        let dir = std::env::temp_dir().join(format!("gna-drive-test-{}", std::process::id()));
        fs::create_dir_all(dir.join("sous")).unwrap();
        let d = Drive::new(dir.clone(), 1 << 20);
        assert_eq!(d.resolve("\\sous\\a.txt"), Some(dir.join("sous/a.txt")));
        assert_eq!(d.resolve("\\"), Some(dir.clone()));
        assert_eq!(d.resolve("\\..\\etc\\passwd"), None);
        assert_eq!(d.resolve("\\sous\\..\\..\\x"), None);
        assert_eq!(d.resolve("/../x"), None);
        assert_eq!(d.resolve("\\a.txt:flux"), None);
        std::os::unix::fs::symlink("/etc", dir.join("lien")).unwrap();
        assert_eq!(d.resolve("\\lien\\passwd"), None);
        assert_eq!(d.resolve("\\lien"), None);
        fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn jokers() {
        assert!(matches("*", "abc.txt"));
        assert!(matches("*.TXT", "abc.txt"));
        assert!(matches("a?c.txt", "abc.txt"));
        assert!(!matches("*.doc", "abc.txt"));
        assert!(matches("abc.txt", "ABC.TXT"));
    }
}
