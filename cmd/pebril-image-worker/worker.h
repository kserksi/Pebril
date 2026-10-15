#ifndef PEBRIL_WORKER_H
#define PEBRIL_WORKER_H
const char *worker_library(void);
int worker_capabilities(void);
int worker_color(const char *,const char *,int);
int worker_minimum_quality(const char *,const char *,const char *);
int worker_animation(const char *,const char *,const char *,const char *,int,int,int,int,int,int *,int *,int *);
#endif
