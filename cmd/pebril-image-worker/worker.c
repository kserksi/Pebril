//go:build image_worker

#define _GNU_SOURCE
#include "worker.h"
#include <vips/vips.h>
#include <dlfcn.h>
#include <math.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

const char *worker_library(void) {Dl_info info;return dladdr(vips_init,&info)?info.dli_fname:"";}
int worker_capabilities(void) {
 const char *ops[]={"pngload","pngsave","jpegload","jpegsave","webpload","webpsave","gifload","resize","autorot","rawsave"};
 for(int i=0;i<10;i++)if(!vips_type_find("VipsOperation",ops[i]))return 0;
 VipsOperation *save=vips_operation_new("webpsave");
 if(!save)return 0;
 int supported=g_object_class_find_property(G_OBJECT_GET_CLASS(save),"mixed")!=NULL;
 g_object_unref(save);return supported;
}
static void clean_metadata(VipsImage *image,int color) {
 char **fields=vips_image_get_fields(image);
 for(int i=0;fields&&fields[i];i++) {
  const char *key=fields[i];
  if(!strcmp(key,"page-height")||!strcmp(key,"n-pages")||!strcmp(key,"delay")||!strcmp(key,"loop")||!strcmp(key,"orientation")||(color&&!strcmp(key,VIPS_META_ICC_NAME)))continue;
  vips_image_remove(image,key);
 }
 if(!color)vips_image_remove(image,VIPS_META_ICC_NAME);
 g_strfreev(fields);
}
int worker_color(const char *input,const char *output,int page) {
 VipsImage *image=page>=0?vips_image_new_from_file(input,"page",page,NULL):vips_image_new_from_file(input,NULL),*converted=NULL;
 if(!image)return 0;
 int has_profile=vips_image_get_typeof(image,VIPS_META_ICC_NAME)!=0;
 int cmyk=vips_image_get_interpretation(image)==VIPS_INTERPRETATION_CMYK;
 if(!has_profile&&!cmyk){g_object_unref(image);return 1;}
 int depth=vips_image_get_format(image)==VIPS_FORMAT_USHORT?16:8;
 int result=vips_icc_transform(image,&converted,"srgb","embedded",TRUE,"input_profile",cmyk?"cmyk":"srgb","depth",depth,NULL);
 if(!result)result=vips_pngsave(converted,output,NULL);
 if(converted)g_object_unref(converted);g_object_unref(image);return result?0:2;
}
int worker_minimum_quality(const char *input,const char *output,const char *format) {
 VipsImage *image=vips_image_new_from_file(input,NULL),*flat=NULL;
 if(!image)return 0;
 int result=-1;
 if(!strcmp(format,"webp"))result=vips_webpsave(image,output,"Q",0,"effort",4,NULL);
 else if(!strcmp(format,"avif"))result=vips_heifsave(image,output,"compression",VIPS_FOREIGN_HEIF_COMPRESSION_AV1,"Q",0,NULL);
 else if(!strcmp(format,"jpeg")) {
  if(vips_image_hasalpha(image)) {
   VipsArrayDouble *background=vips_array_double_newv(3,255.0,255.0,255.0);
   result=vips_flatten(image,&flat,"background",background,NULL);vips_area_unref((VipsArea *)background);
   if(result){g_object_unref(image);return 0;}
  }
  // JPEG's encoder minimum is one; zero selects that minimum.
  result=vips_jpegsave(flat?flat:image,output,"Q",1,"interlace",TRUE,NULL);
 }
 if(flat)g_object_unref(flat);g_object_unref(image);return result==0;
}
int worker_animation(const char *input,const char *output,const char *rust,const char *format,int quality,int width,int height,int orientation,int color,int *out_w,int *out_h,int *out_n) {
 VipsImage *image=vips_image_new_from_file(input,"n",-1,"access",VIPS_ACCESS_RANDOM,"fail_on",VIPS_FAIL_ON_WARNING,NULL);
 int ok=0,*delay=NULL,delay_n=0,loop=0,n=0,page=0;
 if(!image)return 0;
 n=vips_image_get_n_pages(image);page=vips_image_get_page_height(image);
 if(n<2||page<=0)goto done;
 vips_image_get_array_int(image,"delay",&delay,&delay_n);vips_image_get_int(image,"loop",&loop);
 // Hold independent timings because replacing a pipeline can release its metadata arrays.
 int *timings=g_new0(int,n);for(int i=0;i<n;i++)timings[i]=(delay&&i<delay_n)?delay[i]:0;
 if(orientation || width>0) {
  // Transform each frame independently, including orientation, before rejoining.
  VipsImage **parts=g_new0(VipsImage*,n),*joined=NULL;
  int count=0;
  for(int i=0;i<n;i++) {
   VipsImage *frame=NULL,*changed=NULL;
   if(vips_extract_area(image,&frame,0,i*page,vips_image_get_width(image),page,NULL))break;
   if(orientation) {
    if(vips_autorot(frame,&changed,NULL)){g_object_unref(frame);break;}
    g_object_unref(frame);frame=changed;changed=NULL;
   }
   double scale=width>0?fmin(1.0,fmin((double)width/vips_image_get_width(frame),(double)height/vips_image_get_height(frame))):1.0;
   if(scale<1.0) {
    if(vips_resize(frame,&changed,scale,"kernel",VIPS_KERNEL_LANCZOS3,NULL)){g_object_unref(frame);break;}
    g_object_unref(frame);frame=changed;
   }
   parts[i]=frame;count++;
  }
  if(count==n && !vips_arrayjoin(parts,&joined,n,"across",1,NULL))page=vips_image_get_height(parts[0]);
  for(int i=0;i<n;i++)if(parts[i])g_object_unref(parts[i]);g_free(parts);
  if(!joined)goto timed_done;
  g_object_unref(image);image=joined;
 }
 if(color && vips_image_get_typeof(image,VIPS_META_ICC_NAME)) {
  VipsImage *converted=NULL;
  if(vips_icc_transform(image,&converted,"srgb","embedded",TRUE,NULL))goto timed_done;
  g_object_unref(image);image=converted;
 }
 if(!orientation)vips_image_remove(image,"orientation");
 vips_image_set_int(image,"page-height",page);vips_image_set_int(image,"n-pages",n);vips_image_set_array_int(image,"delay",timings,n);vips_image_set_int(image,"loop",loop);
 clean_metadata(image,color);
 if(!strcmp(format,"webp")) {
  ok=vips_webpsave(image,output,"Q",quality,"mixed",TRUE,"effort",4,NULL)==0;
 } else if(!strcmp(format,"gif")) {
  VipsImage *rgb=NULL,*rgba=NULL,*bytes=NULL;
  if(vips_colourspace(image,&rgb,VIPS_INTERPRETATION_sRGB,NULL))goto timed_done;
  if(vips_image_hasalpha(rgb)){rgba=rgb;rgb=NULL;}else {if(vips_addalpha(rgb,&rgba,NULL)){g_object_unref(rgb);goto timed_done;}g_object_unref(rgb);}
  if(vips_cast(rgba,&bytes,VIPS_FORMAT_UCHAR,NULL)){g_object_unref(rgba);goto timed_done;}g_object_unref(rgba);
  char raw[4096];if(snprintf(raw,sizeof(raw),"%s.rgba",output)>=(int)sizeof(raw)){g_object_unref(bytes);goto timed_done;}
  if(vips_rawsave(bytes,raw,NULL)){g_object_unref(bytes);goto timed_done;}g_object_unref(bytes);
  void *handle=dlopen(rust,RTLD_NOW|RTLD_LOCAL);
  unsigned int (*encode)(const char*,const char*,unsigned int,unsigned int,unsigned int,const int*,unsigned int)=handle?dlsym(handle,"pebril_encode_gif"):NULL;
  if(encode)ok=encode(raw,output,vips_image_get_width(image),page,n,timings,loop)!=0;
  unlink(raw);if(handle)dlclose(handle);
 }
 if(ok){*out_w=vips_image_get_width(image);*out_h=page;*out_n=n;}
 timed_done:g_free(timings);
 done:if(image)g_object_unref(image);return ok;
}
