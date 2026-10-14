#ifndef PEBRIL_BRIDGE_H
#define PEBRIL_BRIDGE_H
const char *probe_glibc_version(void);
int probe_vips(const char *library,const char *directory,int avif,char *version,int version_size,char *loaded,int loaded_size,char *error,int error_size);
int probe_rust(const char *library,char *error,int error_size);
int verify_raster(const char *library,const char *input,long long limit,int *width,int *height,int *frames,char *format,int format_size,char *error,int error_size);
int verify_rust_file(const char *library,const char *input,const char *output,int svg,unsigned long long limit,unsigned long long bytes,int *width,int *height,int *frames);
#endif
