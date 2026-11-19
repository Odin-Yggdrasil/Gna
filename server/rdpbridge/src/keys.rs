//! Conversion des événements clavier VNC (keysyms X11) en événements RDP.
//!
//! Principe :
//!  - les caractères imprimables sont envoyés en **Unicode** : le bon caractère arrive
//!    sur la cible quelle que soit la disposition de son clavier (é, @, €…) ;
//!  - les touches spéciales (Entrée, flèches, F1…) et les modificateurs sont envoyés
//!    en **scancodes** ;
//!  - avec Ctrl, Alt ou Windows enfoncé, les lettres et chiffres partent en scancodes
//!    (raccourcis Ctrl+C, Alt+F4…), selon la disposition déclarée pour la cible.

use std::collections::HashMap;

use ironrdp::input::{Operation, Scancode};

/// Disposition du clavier de la cible (pour les raccourcis).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Layout {
    /// AZERTY Windows (~ et ` sont des touches mortes).
    Fr,
    /// AZERTY Linux / xrdp (AltGr+2 et AltGr+7 donnent ~ et ` directement).
    FrLinux,
    Us,
}

impl Layout {
    pub fn parse(s: &str) -> Self {
        match s {
            "us" => Layout::Us,
            "fr-linux" => Layout::FrLinux,
            _ => Layout::Fr,
        }
    }
    /// Identifiant de disposition Windows annoncé à la connexion.
    pub fn windows_id(self) -> u32 {
        match self {
            Layout::Fr | Layout::FrLinux => 0x040C,
            Layout::Us => 0x0409,
        }
    }
}

const fn sc(code: u8) -> Scancode {
    Scancode::from_u8(false, code)
}
const fn ext(code: u8) -> Scancode {
    Scancode::from_u8(true, code)
}

/// Touches spéciales : keysym X11 → scancode.
fn special(keysym: u32) -> Option<Scancode> {
    Some(match keysym {
        0xff08 => sc(0x0E),                 // BackSpace
        0xff09 | 0xfe20 => sc(0x0F),        // Tab, ISO_Left_Tab
        0xff0d => sc(0x1C),                 // Return
        0xff1b => sc(0x01),                 // Escape
        0xffff => ext(0x53),                // Delete
        0xff50 => ext(0x47),                // Home
        0xff51 => ext(0x4B),                // Left
        0xff52 => ext(0x48),                // Up
        0xff53 => ext(0x4D),                // Right
        0xff54 => ext(0x50),                // Down
        0xff55 => ext(0x49),                // Page_Up
        0xff56 => ext(0x51),                // Page_Down
        0xff57 => ext(0x4F),                // End
        0xff63 => ext(0x52),                // Insert
        0xff61 => ext(0x37),                // Print
        0xff67 => ext(0x5D),                // Menu
        0xff13 => sc(0x45),                 // Pause
        0xff14 => sc(0x46),                 // Scroll_Lock
        0xff7f => sc(0x45),                 // Num_Lock
        0xffe5 => sc(0x3A),                 // Caps_Lock
        0xff8d => ext(0x1C),                // KP_Enter
        0x0020 => sc(0x39),                 // espace
        k @ 0xffbe..=0xffc7 => sc(0x3B + (k - 0xffbe) as u8), // F1–F10
        0xffc8 => sc(0x57),                 // F11
        0xffc9 => sc(0x58),                 // F12
        0xffe1 => sc(0x2A),                 // Shift_L
        0xffe2 => sc(0x36),                 // Shift_R
        0xffe3 => sc(0x1D),                 // Control_L
        0xffe4 => ext(0x1D),                // Control_R
        0xffe9 | 0xffe7 => sc(0x38),        // Alt_L, Meta_L
        0xffea | 0xffe8 => ext(0x38),       // Alt_R, Meta_R
        0xffeb => ext(0x5B),                // Super_L (Windows)
        0xffec => ext(0x5C),                // Super_R
        _ => return None,
    })
}

/// Lettres et chiffres → scancodes, selon la disposition (pour Ctrl/Alt + touche).
fn shortcut_scancode(c: char, layout: Layout) -> Option<Scancode> {
    let c = c.to_ascii_lowercase();
    // Rangée de chiffres : mêmes positions physiques en QWERTY et AZERTY.
    if let Some(d) = c.to_digit(10) {
        return Some(sc(if d == 0 { 0x0B } else { 0x01 + d as u8 }));
    }
    let qwerty = match c {
        'q' => 0x10, 'w' => 0x11, 'e' => 0x12, 'r' => 0x13, 't' => 0x14, 'y' => 0x15, 'u' => 0x16,
        'i' => 0x17, 'o' => 0x18, 'p' => 0x19, 'a' => 0x1E, 's' => 0x1F, 'd' => 0x20, 'f' => 0x21,
        'g' => 0x22, 'h' => 0x23, 'j' => 0x24, 'k' => 0x25, 'l' => 0x26, 'z' => 0x2C, 'x' => 0x2D,
        'c' => 0x2E, 'v' => 0x2F, 'b' => 0x30, 'n' => 0x31, 'm' => 0x32,
        _ => return None,
    };
    let azerty = layout != Layout::Us;
    let code = match (azerty, c) {
        // AZERTY : A↔Q, Z↔W échangés, M à la place du point-virgule QWERTY.
        (true, 'a') => 0x10,
        (true, 'q') => 0x1E,
        (true, 'z') => 0x11,
        (true, 'w') => 0x2C,
        (true, 'm') => 0x27,
        _ => qwerty,
    };
    Some(sc(code))
}

/// Touche produisant un caractère : (scancode, Maj requis, AltGr requis).
type KeyPos = (u8, bool, bool);

/// Position d'un caractère sur le clavier de la cible. `None` : caractère absent
/// (touches mortes, symboles rares) → envoi en Unicode.
fn char_position(c: char, layout: Layout) -> Option<KeyPos> {
    if c == ' ' {
        return Some((0x39, false, false));
    }
    if c.is_ascii_alphabetic() {
        let code = shortcut_scancode(c, layout)?.as_u8().1;
        return Some((code, c.is_ascii_uppercase(), false));
    }
    match layout {
        Layout::Us => {
            let row = "1234567890-=";
            let row_s = "!@#$%^&*()_+";
            if let Some(i) = row.find(c) {
                return Some((0x02 + i as u8, false, false));
            }
            if let Some(i) = row_s.find(c) {
                return Some((0x02 + i as u8, true, false));
            }
            Some(match c {
                '[' => (0x1A, false, false), '{' => (0x1A, true, false),
                ']' => (0x1B, false, false), '}' => (0x1B, true, false),
                '\\' => (0x2B, false, false), '|' => (0x2B, true, false),
                ';' => (0x27, false, false), ':' => (0x27, true, false),
                '\'' => (0x28, false, false), '"' => (0x28, true, false),
                '`' => (0x29, false, false), '~' => (0x29, true, false),
                ',' => (0x33, false, false), '<' => (0x33, true, false),
                '.' => (0x34, false, false), '>' => (0x34, true, false),
                '/' => (0x35, false, false), '?' => (0x35, true, false),
                _ => return None,
            })
        }
        Layout::Fr | Layout::FrLinux => {
            if layout == Layout::FrLinux {
                match c {
                    '~' => return Some((0x03, false, true)),
                    '`' => return Some((0x08, false, true)),
                    _ => {}
                }
            }
            // Rangée des chiffres AZERTY : sans Maj, avec Maj, avec AltGr.
            let plain = ['&', 'é', '"', '\'', '(', '-', 'è', '_', 'ç', 'à', ')', '='];
            let shift = ['1', '2', '3', '4', '5', '6', '7', '8', '9', '0', '°', '+'];
            let altgr = [None, None, Some('#'), Some('{'), Some('['), Some('|'), None, Some('\\'), Some('^'), Some('@'), Some(']'), Some('}')];
            for i in 0..12 {
                let code = 0x02 + i as u8;
                if plain[i] == c {
                    return Some((code, false, false));
                }
                if shift[i] == c {
                    return Some((code, true, false));
                }
                if altgr[i] == Some(c) {
                    return Some((code, false, true));
                }
            }
            Some(match c {
                '²' => (0x29, false, false),
                '$' => (0x1B, false, false), '£' => (0x1B, true, false), '¤' => (0x1B, false, true),
                '€' => (0x12, false, true),
                'ù' => (0x28, false, false), '%' => (0x28, true, false),
                '*' => (0x2B, false, false), 'µ' => (0x2B, true, false),
                '<' => (0x56, false, false), '>' => (0x56, true, false),
                ',' => (0x32, false, false), '?' => (0x32, true, false),
                ';' => (0x33, false, false), '.' => (0x33, true, false),
                ':' => (0x34, false, false), '/' => (0x34, true, false),
                '!' => (0x35, false, false), '§' => (0x35, true, false),
                _ => return None,
            })
        }
    }
}

/// Suite de frappes produisant un caractère : touche directe, ou touche morte suivie
/// de la lettre de base (ê = ^ puis e, ñ = ~ puis n…).
fn char_sequence(c: char, layout: Layout) -> Option<Vec<KeyPos>> {
    if let Some(p) = char_position(c, layout) {
        return Some(vec![p]);
    }
    if layout == Layout::Us {
        return None;
    }
    let dead_tilde_grave = layout == Layout::Fr; // absentes de l'AZERTY Linux
    // Touches mortes AZERTY : circonflexe, tréma, tilde (AltGr+2), grave (AltGr+7).
    const CIRC: KeyPos = (0x1A, false, false);
    const TREMA: KeyPos = (0x1A, true, false);
    const TILDE: KeyPos = (0x03, false, true);
    const GRAVE: KeyPos = (0x08, false, true);
    let (dead, base) = match c {
        '~' => (TILDE, ' '),
        '`' => (GRAVE, ' '),
        'â' | 'ê' | 'î' | 'ô' | 'û' | 'Â' | 'Ê' | 'Î' | 'Ô' | 'Û' => (CIRC, strip(c)),
        'ä' | 'ë' | 'ï' | 'ö' | 'ü' | 'ÿ' | 'Ä' | 'Ë' | 'Ï' | 'Ö' | 'Ü' => (TREMA, strip(c)),
        'ã' | 'ñ' | 'õ' | 'Ã' | 'Ñ' | 'Õ' if dead_tilde_grave => (TILDE, strip(c)),
        'ò' | 'ì' | 'À' | 'È' | 'Ù' | 'Ò' | 'Ì' if dead_tilde_grave => (GRAVE, strip(c)),
        _ => return None,
    };
    Some(vec![dead, char_position(base, layout)?])
}

/// Lettre de base d'une lettre accentuée (casse conservée).
fn strip(c: char) -> char {
    let base = match c.to_lowercase().next().unwrap_or(c) {
        'â' | 'ä' | 'ã' | 'à' => 'a',
        'ê' | 'ë' | 'è' => 'e',
        'î' | 'ï' | 'ì' => 'i',
        'ô' | 'ö' | 'õ' | 'ò' => 'o',
        'û' | 'ü' | 'ù' => 'u',
        'ÿ' => 'y',
        'ñ' => 'n',
        other => other,
    };
    if c.is_uppercase() { base.to_ascii_uppercase() } else { base }
}

/// Caractère correspondant à un keysym imprimable.
fn keysym_char(keysym: u32) -> Option<char> {
    match keysym {
        0x20..=0x7e | 0xa0..=0xff => char::from_u32(keysym),
        0x0100_0100..=0x0110_ffff => char::from_u32(keysym - 0x0100_0000),
        0x20ac => Some('€'), // EuroSign (keysym historique)
        _ => None,
    }
}

#[derive(Clone, Copy)]
enum Pressed {
    Scan(Scancode),
    Unicode(char),
    Ignored,
}

/// État du clavier côté passerelle.
pub struct Keyboard {
    layout: Layout,
    pressed: HashMap<u32, Pressed>,
    ctrl: u8,
    alt: u8,
    meta: u8,
    shift_l: bool, // Maj gauche / droite enfoncées (transmises à la cible)
    shift_r: bool,
}

impl Keyboard {
    pub fn new(layout: Layout) -> Self {
        Keyboard { layout, pressed: HashMap::new(), ctrl: 0, alt: 0, meta: 0, shift_l: false, shift_r: false }
    }

    fn track(&mut self, keysym: u32, down: bool) {
        match keysym {
            0xffe1 => self.shift_l = down,
            0xffe2 => self.shift_r = down,
            _ => {}
        }
        let counter = match keysym {
            0xffe3 | 0xffe4 => &mut self.ctrl,
            0xffe9 | 0xffe7 => &mut self.alt, // Alt gauche seulement (AltGr sert aux caractères)
            0xffeb | 0xffec => &mut self.meta,
            _ => return,
        };
        *counter = if down { counter.saturating_add(1) } else { counter.saturating_sub(1) };
    }

    /// Traduit un événement clavier VNC en opérations RDP.
    pub fn event(&mut self, keysym: u32, down: bool) -> Vec<Operation> {
        if !down {
            // Relâcher exactement ce qui a été appuyé (même si les modificateurs ont changé).
            self.track(keysym, false);
            return match self.pressed.remove(&keysym) {
                Some(Pressed::Scan(s)) => vec![Operation::KeyReleased(s)],
                Some(Pressed::Unicode(c)) => vec![Operation::UnicodeKeyReleased(c)],
                _ => vec![],
            };
        }

        // AltGr (ISO_Level3_Shift, Mode_switch) n'est jamais transmis : les caractères
        // qu'il produit (@, €, #…) arrivent déjà résolus en Unicode.
        if keysym == 0xfe03 || keysym == 0xff7e {
            self.pressed.insert(keysym, Pressed::Ignored);
            return vec![];
        }

        // Ctrl+Alt+Suppr est transmis tel quel : c'est ce qu'envoie aussi le client
        // Bureau à distance de Windows (qui le déclenche par Ctrl+Alt+Fin en local).
        let keysym_eff = keysym;

        let modifier_held = self.ctrl > 0 || self.alt > 0 || self.meta > 0;
        let action = if let Some(s) = special(keysym_eff) {
            Pressed::Scan(s)
        } else if let Some(c) = keysym_char(keysym) {
            match (modifier_held, shortcut_scancode(c, self.layout)) {
                (true, Some(s)) => Pressed::Scan(s),
                _ => match char_sequence(c, self.layout) {
                    // Frappe complète synthétisée à l'appui (Maj/AltGr ajustés autour) ;
                    // le relâchement envoyé ensuite par le client est ignoré.
                    Some(seq) => {
                        self.pressed.insert(keysym, Pressed::Ignored);
                        return seq.into_iter().flat_map(|p| self.tap(p)).collect();
                    }
                    None => Pressed::Unicode(c),
                },
            }
        } else {
            Pressed::Ignored
        };
        self.track(keysym, true);
        self.pressed.insert(keysym, action);
        match action {
            Pressed::Scan(s) => vec![Operation::KeyPressed(s)],
            Pressed::Unicode(c) => vec![Operation::UnicodeKeyPressed(c)],
            Pressed::Ignored => vec![],
        }
    }

    /// Frappe d'une touche avec l'état Maj/AltGr voulu, puis retour à l'état précédent.
    fn tap(&self, (code, shift, altgr): KeyPos) -> Vec<Operation> {
        let mut before = Vec::new();
        let mut after = Vec::new();
        let shift_held = self.shift_l || self.shift_r;
        if shift && !shift_held {
            before.push(Operation::KeyPressed(sc(0x2A)));
            after.push(Operation::KeyReleased(sc(0x2A)));
        } else if !shift && shift_held {
            for (held, k) in [(self.shift_l, sc(0x2A)), (self.shift_r, sc(0x36))] {
                if held {
                    before.push(Operation::KeyReleased(k));
                    after.push(Operation::KeyPressed(k));
                }
            }
        }
        if altgr {
            before.push(Operation::KeyPressed(ext(0x38)));
            after.insert(0, Operation::KeyReleased(ext(0x38)));
        }
        let mut ops = before;
        ops.push(Operation::KeyPressed(sc(code)));
        ops.push(Operation::KeyReleased(sc(code)));
        ops.extend(after);
        ops
    }

    /// Relâche tout (fin de session, perte de focus).
    pub fn release_all(&mut self) -> Vec<Operation> {
        let ops = self
            .pressed
            .drain()
            .filter_map(|(_, p)| match p {
                Pressed::Scan(s) => Some(Operation::KeyReleased(s)),
                Pressed::Unicode(c) => Some(Operation::UnicodeKeyReleased(c)),
                Pressed::Ignored => None,
            })
            .collect();
        self.ctrl = 0;
        self.alt = 0;
        self.meta = 0;
        self.shift_l = false;
        self.shift_r = false;
        ops
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Operation n'implémente pas PartialEq : comparaison par représentation.
    fn first(ops: Vec<Operation>) -> String {
        format!("{:?}", ops.into_iter().next().expect("une opération"))
    }
    fn d(op: Operation) -> String {
        format!("{op:?}")
    }

    fn all(ops: Vec<Operation>) -> Vec<String> {
        ops.into_iter().map(|o| format!("{o:?}")).collect()
    }

    #[test]
    fn azerty_chars_use_scancodes() {
        let mut k = Keyboard::new(Layout::Fr);
        // « a » sur AZERTY = touche 0x10, sans Maj
        assert_eq!(all(k.event('a' as u32, true)), vec![d(Operation::KeyPressed(sc(0x10))), d(Operation::KeyReleased(sc(0x10)))]);
        assert!(k.event('a' as u32, false).is_empty(), "relâchement déjà envoyé");
        // « 1 » = Maj + touche 0x02 (Maj synthétisé)
        assert_eq!(all(k.event('1' as u32, true)).len(), 4);
        // « @ » = AltGr + touche 0x0B ; AltGr physique jamais transmis
        assert!(k.event(0xfe03, true).is_empty());
        let at = all(k.event('@' as u32, true));
        assert_eq!(at.first(), Some(&d(Operation::KeyPressed(ext(0x38)))));
        assert!(at.contains(&d(Operation::KeyPressed(sc(0x0B)))));
        // « ê » = touche morte ^ puis e
        let e = all(k.event('ê' as u32, true));
        assert_eq!(e, vec![
            d(Operation::KeyPressed(sc(0x1A))), d(Operation::KeyReleased(sc(0x1A))),
            d(Operation::KeyPressed(sc(0x12))), d(Operation::KeyReleased(sc(0x12))),
        ]);
        // « É » n'existe sur aucune touche AZERTY : Unicode
        assert_eq!(first(k.event('É' as u32, true)), d(Operation::UnicodeKeyPressed('É')));
    }

    #[test]
    fn linux_azerty_tilde_is_direct() {
        let mut k = Keyboard::new(Layout::FrLinux);
        let t = all(k.event('~' as u32, true));
        assert_eq!(t.len(), 4, "AltGr + touche, sans espace : {t:?}");
        let mut w = Keyboard::new(Layout::Fr);
        assert_eq!(all(w.event('~' as u32, true)).len(), 6, "Windows : touche morte (AltGr+2) puis espace");
    }

    #[test]
    fn shift_is_adjusted() {
        let mut k = Keyboard::new(Layout::Us);
        k.event(0xffe1, true); // Maj gauche tenu
        // « 1 » sur QWERTY ne veut pas Maj : relâché puis rétabli autour de la frappe
        assert_eq!(
            all(k.event('1' as u32, true)),
            vec![
                d(Operation::KeyReleased(sc(0x2A))),
                d(Operation::KeyPressed(sc(0x02))),
                d(Operation::KeyReleased(sc(0x02))),
                d(Operation::KeyPressed(sc(0x2A))),
            ]
        );
        // « A » avec Maj tenu : pas de Maj supplémentaire
        assert_eq!(all(k.event('A' as u32, true)).len(), 2);
    }

    #[test]
    fn unmapped_chars_fall_back_to_unicode() {
        let mut k = Keyboard::new(Layout::Us);
        assert_eq!(first(k.event('é' as u32, true)), d(Operation::UnicodeKeyPressed('é')));
        assert_eq!(first(k.event('é' as u32, false)), d(Operation::UnicodeKeyReleased('é')));
    }

    #[test]
    fn shortcuts_follow_layout() {
        let mut k = Keyboard::new(Layout::Fr);
        k.event(0xffe3, true); // Ctrl
        assert_eq!(first(k.event('a' as u32, true)), d(Operation::KeyPressed(sc(0x10))));
        k.event(0xffe3, false); // Ctrl relâché avant la lettre
        assert_eq!(first(k.event('a' as u32, false)), d(Operation::KeyReleased(sc(0x10))));

        let mut us = Keyboard::new(Layout::Us);
        us.event(0xffe3, true);
        assert_eq!(first(us.event('a' as u32, true)), d(Operation::KeyPressed(sc(0x1E))));
    }

    #[test]
    fn ctrl_alt_del_transmis() {
        let mut k = Keyboard::new(Layout::Us);
        k.event(0xffe3, true);
        k.event(0xffe9, true);
        assert_eq!(first(k.event(0xffff, true)), d(Operation::KeyPressed(ext(0x53))));
    }
}
