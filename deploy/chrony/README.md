# AWS clock diagnostics

These operator-selected profiles are included in the immutable release. The
installer preserves the host's existing Chrony configuration. Apply a profile
only on a disposable AWS test host, with the gateway stopped and a reviewed
run plan. Do not configure the operator's workstation with these files.

`aws.conf` preserves the previous AWS source and polling settings, adding native
measurement/statistics/tracking logs. Capture this baseline before changing it.
`aws-burst.conf` is a diagnostic candidate, not an accepted fix: it allows shorter
minimum polling and recovery bursts when a good measurement is unavailable.
The candidate permits up to four requests per burst at polling level 4. A
minimum polling level of 3 permits only two; live testing exhausted those two
attempts on consecutive delay-filter rejections and observed stale tracking.
Chrony's delay filters and the runtime's freshness/uncertainty guards stay intact.
See [AWS configuration](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configure-ec2-ntp.html)
and [Chrony burst/log directives](https://chrony-project.org/doc/4.3/chrony.conf.html).

Before restarting Chrony, save its version, configuration/hash and counters, and
validate the candidate with `chronyd -p -f <profile>`. Save logs before any
restart/rotation; track `chronyc ntpdata`, tracking, sources and sourcestats in the
same UTC window as client results and gateway logs. Polling or packet receipt
alone does not prove that Chrony accepted a new measurement.

Only compare the candidate after measurement logs identify the baseline failure.
Restart the gateway after clock bounds settle, use fresh trial identities and run
C1 before C4 on the same release/config. A successful retry does not erase a
failed baseline trial. Retain the original configuration for rollback and export
the native logs before deleting the host.
