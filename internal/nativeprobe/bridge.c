#define _GNU_SOURCE
#include "bridge.h"
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <gnu/libc-version.h>
const char *probe_glibc_version(void) { return gnu_get_libc_version(); }

// Call only in the isolated project self-test child; third-party libraries are not standalone tools.
int probe_vips(const char *library,const char *directory,int avif,char *version,int vs,char *loaded,int ls,char *error,int es) {
 void *handle=dlopen(library,RTLD_NOW|RTLD_LOCAL);
 if(!handle){snprintf(error,es,"Cannot load: %s",dlerror());return 0;}
 #define SYM(type,name) type name=(type)dlsym(handle,#name); if(!name){snprintf(error,es,"Missing ABI symbol %s",#name);return 0;}
 typedef int (*component_fn)(int);
 typedef int (*init_fn)(const char*); typedef const char* (*str_fn)(void);
 typedef unsigned long (*find_fn)(const char*,const char*);
 typedef void* (*memory_fn)(const void*,size_t,int,int,int,int);
 typedef int (*write_fn)(void*,const char*,...);
 typedef void* (*read_fn)(const char*,...);
 typedef void* (*pixels_fn)(void*,size_t*);
 typedef int (*size_fn)(const void*); typedef void (*free_fn)(void*);
 SYM(component_fn,vips_version); SYM(init_fn,vips_init); SYM(str_fn,vips_version_string); SYM(str_fn,vips_error_buffer);
 SYM(find_fn,vips_type_find); SYM(memory_fn,vips_image_new_from_memory);
 SYM(write_fn,vips_image_write_to_file); SYM(read_fn,vips_image_new_from_file);
 SYM(pixels_fn,vips_image_write_to_memory); SYM(size_fn,vips_image_get_width); SYM(size_fn,vips_image_get_height);
 SYM(free_fn,g_object_unref); SYM(free_fn,g_free);
 snprintf(version,vs,"%s",vips_version_string());
 int major=vips_version(0),minor=vips_version(1),patch=vips_version(2);
 if(major<8||(major==8&&(minor<16||(minor==16&&patch<1)))){snprintf(error,es,"Compatible libvips >=8.16.1 required; found %s",version);return 0;}
 if(vips_init("pebril-probe")){snprintf(error,es,"Initialization failed: %s",vips_error_buffer());return 0;}
 Dl_info info;if(dladdr((void*)vips_init,&info))snprintf(loaded,ls,"%s",info.dli_fname);
 const char *ops[]={"pngload","pngsave","jpegload","jpegsave","webpload","webpsave","gifload"};
 for(size_t i=0;i<sizeof(ops)/sizeof(ops[0]);i++)if(!vips_type_find("VipsOperation",ops[i])){snprintf(error,es,"Missing operation %s",ops[i]);return 0;}
 if(avif&&(!vips_type_find("VipsOperation","heifload")||!vips_type_find("VipsOperation","heifsave"))){snprintf(error,es,"AVIF is enabled but HEIF/AVIF codecs are missing");return 0;}
 unsigned char pixels[12]={255,0,0,0,255,0,0,0,255,255,255,255};
 void *image=vips_image_new_from_memory(pixels,sizeof(pixels),2,2,3,0);
 if(!image){snprintf(error,es,"Failed to create test image");return 0;}
 const char *extensions[]={"png","jpg","webp","avif"};
 int passed=1;
 for(int i=0;i<(avif?4:3);i++){
  char path[4096];int length=snprintf(path,sizeof(path),"%s/probe.%s",directory,extensions[i]);
  if(length<0||length>=(int)sizeof(path)){snprintf(error,es,"Self-test path is too long");passed=0;break;}
  if(vips_image_write_to_file(image,path,NULL)){snprintf(error,es,"Failed to encode %s: %s",extensions[i],vips_error_buffer());passed=0;unlink(path);break;}
  void *decoded=vips_image_new_from_file(path,NULL);size_t n=0;void *raw=NULL;
  if(decoded)raw=vips_image_write_to_memory(decoded,&n);
  if(!decoded||!raw||n==0||vips_image_get_width(decoded)!=2||vips_image_get_height(decoded)!=2){snprintf(error,es,"Failed to decode %s: %s",extensions[i],vips_error_buffer());passed=0;}
  if(raw)g_free(raw);if(decoded)g_object_unref(decoded);unlink(path);if(!passed)break;
 }
 g_object_unref(image);return passed;
}
int probe_rust(const char *library,char *error,int es){
 void *handle=dlopen(library,RTLD_NOW|RTLD_LOCAL);if(!handle){snprintf(error,es,"Cannot load internal Rust library: %s",dlerror());return 0;}
 unsigned int (*abi)(void)=dlsym(handle,"pebril_native_abi");
 unsigned int (*probe)(void)=dlsym(handle,"pebril_native_probe");
 if(!abi||!probe||abi()!=1){snprintf(error,es,"Incompatible internal Rust library ABI");return 0;}
 unsigned int bits=probe();if(bits!=3){snprintf(error,es,"Rust sanitization/animation self-test failed; capability bits %u",bits);return 0;}return 1;
}
