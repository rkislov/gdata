Name: gdata-srk
Version: 0.9.2
Release: alt1
Summary: Distributed backup system with a web interface
License: Apache-2.0
Group: Archiving/Backup
Url: https://github.com/rkislov/gdata
Source: %{name}-%{version}.tar.gz
BuildArch: x86_64
Requires(pre): shadow-utils
Requires(post): systemd
Requires(preun): systemd
AutoReq: no
AutoProv: no
%define debug_package %{nil}
%define _unpackaged_files_terminate_build 1

%description
GData SRK manager, media server, agent and proxy. Includes a static Go
binary and embedded web UI. PostgreSQL and deployment credentials must
be configured separately. Services are not started automatically.

%prep
%setup -q

%build
# Static binary is built from the matching source tree by build-altlinux.sh.

%install
install -Dm755 srk %buildroot%_bindir/srk
install -Dm644 srk@.service %buildroot%_unitdir/srk@.service
mkdir -p %buildroot%_sysconfdir/srk %buildroot/var/lib/srk %buildroot/var/log/srk
mkdir -p %buildroot%_docdir/%name-%version
cp -a docs LICENSE NOTICE README.md third_party %buildroot%_docdir/%name-%version/

%pre
getent group srk >/dev/null || groupadd -r srk
getent passwd srk >/dev/null || useradd -r -g srk -d /var/lib/srk -s /sbin/nologin -c 'GData SRK' srk

%post
systemctl daemon-reload >/dev/null 2>&1 || :

%preun
if [ "$1" = 0 ]; then
  for role in manager media agent proxy; do
    systemctl disable --now "srk@$role.service" >/dev/null 2>&1 || :
  done
fi

%postun
systemctl daemon-reload >/dev/null 2>&1 || :

%files
%_bindir/srk
%_unitdir/srk@.service
%dir %attr(0750,root,srk) %_sysconfdir/srk
%dir %attr(0750,srk,srk) /var/lib/srk
%dir %attr(0750,srk,srk) /var/log/srk
%_docdir/%name-%version

%changelog
* Mon Nov 02 2026 Roman Kislov <roman@kislovs.ru> 0.9.2-alt1
- Initial ALT Linux 10 (p10) x86_64 package.
