#ifndef PEBRIL_BRIDGE_H
#define PEBRIL_BRIDGE_H
const char *probe_glibc_version(void);
int probe_vips(const char *library,const char *directory,int avif,char *version,int version_size,char *loaded,int loaded_size,char *error,int error_size);
int probe_rust(const char *library,char *error,int error_size);
#endif
