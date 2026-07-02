use std::{collections::HashSet, net::Ipv4Addr};

#[derive(Clone, Debug)]
pub struct Whitelist {
    ips: HashSet<Ipv4Addr>,
    domain: String,
}

impl Whitelist {
    pub fn new(ips: impl IntoIterator<Item = Ipv4Addr>, domain: impl Into<String>) -> Self {
        let domain = domain
            .into()
            .trim()
            .trim_end_matches('.')
            .to_ascii_lowercase();
        Self {
            ips: ips.into_iter().collect(),
            domain,
        }
    }

    pub fn contains(&self, ip: &Ipv4Addr) -> bool {
        self.ips.contains(ip)
    }

    pub fn hostname_for(&self, ip: Ipv4Addr) -> String {
        format!("{ip}.{}", self.domain)
    }
}

pub fn ipv4_is_allowed(ip: Ipv4Addr, allow_private_ip: bool) -> bool {
    if allow_private_ip {
        return !ip.is_unspecified() && !ip.is_broadcast() && !ip.is_multicast();
    }

    let octets = ip.octets();
    let is_private = ip.is_private();
    let is_loopback = ip.is_loopback();
    let is_link_local = ip.is_link_local();
    let is_documentation = matches!(
        octets,
        [192, 0, 2, _] | [198, 51, 100, _] | [203, 0, 113, _]
    );
    let is_shared_address_space = octets[0] == 100 && (64..=127).contains(&octets[1]);
    let is_benchmark = octets[0] == 198 && matches!(octets[1], 18 | 19);
    let is_reserved = octets[0] == 0 || octets[0] >= 240;

    !(is_private
        || is_loopback
        || is_link_local
        || ip.is_broadcast()
        || ip.is_multicast()
        || is_documentation
        || is_shared_address_space
        || is_benchmark
        || is_reserved)
}
